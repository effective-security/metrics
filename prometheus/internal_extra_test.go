package prometheus

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/effective-security/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// counterValue reads the current value of the counter.
func counterValue(t *testing.T, c *counter) float64 {
	t.Helper()
	var pb dto.Metric
	require.NoError(t, c.Write(&pb))
	return pb.GetCounter().GetValue()
}

// collected drains what the sink collects at the given time.
func collected(t *testing.T, sink *Sink, at time.Time) []string {
	t.Helper()
	ch := make(chan prometheus.Metric, 32)
	sink.collectAtTime(ch, at)
	close(ch)

	names := make([]string, 0, len(ch))
	for m := range ch {
		names = append(names, m.Desc().String())
	}
	return names
}

func TestCounterExpiration(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:              "counter_expiry_sink",
		Registerer:        prometheus.NewRegistry(),
		Expiration:        time.Second,
		CounterExpiration: time.Second,
		CounterDefinitions: []CounterDefinition{
			{
				Name: "declared_counter",
				Help: "declared_counter is pre-declared and never expires",
			},
		},
	})
	require.NoError(t, err)

	now := time.Now()
	sink.IncrCounter("runtime_counter", 1, nil)

	require.Len(t, collected(t, sink, now), 2, "both counters are collected while fresh")

	// well past the expiration: the runtime counter goes, the declared one stays
	names := collected(t, sink, now.Add(time.Hour))
	require.Len(t, names, 1)
	assert.Contains(t, names[0], "declared_counter")

	_, hash := flattenKey("runtime_counter", nil)
	_, ok := sink.counters.Load(hash)
	assert.False(t, ok, "the expired counter must be removed from the sink")
}

func TestCounterRetainedByDefault(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:       "counter_retained_sink",
		Registerer: prometheus.NewRegistry(),
		Expiration: time.Second,
		// CounterExpiration is not set
	})
	require.NoError(t, err)

	now := time.Now()
	sink.IncrCounter("runtime_counter", 1, nil)
	sink.SetGauge("runtime_gauge", 1, nil)

	require.Len(t, collected(t, sink, now), 2)

	// the gauge expires, the counter is kept
	names := collected(t, sink, now.Add(time.Hour))
	require.Len(t, names, 1)
	assert.Contains(t, names[0], "runtime_counter")
}

func TestEmitUpdatesInPlace(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:       "in_place_sink",
		Registerer: prometheus.NewRegistry(),
		Expiration: time.Minute,
	})
	require.NoError(t, err)

	sink.IncrCounter("runtime_counter", 1, nil)
	_, hash := flattenKey("runtime_counter", nil)
	first, ok := sink.counters.Load(hash)
	require.True(t, ok)

	sink.IncrCounter("runtime_counter", 1, nil)
	second, ok := sink.counters.Load(hash)
	require.True(t, ok)

	assert.Same(t, first, second, "an update must not replace the map entry")
	assert.Equal(t, float64(2), counterValue(t, first.(*counter)))
}

func TestRetireDoesNotDropAConcurrentUpdate(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:              "retire_race_sink",
		Registerer:        prometheus.NewRegistry(),
		CounterExpiration: time.Nanosecond,
	})
	require.NoError(t, err)

	sink.IncrCounter("runtime_counter", 1, nil)
	_, hash := flattenKey("runtime_counter", nil)
	v, ok := sink.counters.Load(hash)
	require.True(t, ok)
	c := v.(*counter)

	future := time.Now().Add(time.Hour)

	// an emission that lands between idleSince and retire keeps the entry
	seen, idle := c.idleSince(time.Nanosecond, future)
	require.True(t, idle)
	require.True(t, c.touch(time.Now()))
	assert.False(t, c.retire(seen), "an entry updated since it was read as idle must be kept")
	assert.True(t, retire(&sink.counters, hash, v, &c.lastUpdate, time.Nanosecond, future))

	// once retired, the entry refuses updates instead of swallowing them
	assert.False(t, c.touch(time.Now()))
	_, ok = sink.counters.Load(hash)
	assert.False(t, ok)

	// so the next emission registers a new series carrying the value
	sink.IncrCounter("runtime_counter", 5, nil)
	next, ok := sink.counters.Load(hash)
	require.True(t, ok)
	assert.NotSame(t, v, next)
	assert.Equal(t, float64(5), counterValue(t, next.(*counter)))
}

func TestEmitWhileCollecting(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:              "emit_while_collecting_sink",
		Registerer:        prometheus.NewRegistry(),
		Expiration:        time.Nanosecond,
		CounterExpiration: time.Nanosecond,
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sink.IncrCounter("runtime_counter", 1, nil)
					sink.SetGauge("runtime_gauge", 1, nil)
					sink.AddSample("runtime_sample", 1, nil)
				}
			}
		}()
	}

	// collect far enough in the future that every series is retired each time
	for range 200 {
		ch := make(chan prometheus.Metric, 32)
		sink.collectAtTime(ch, time.Now().Add(time.Hour))
		close(ch)
		for range ch {
			// drain what the collector produced
		}
	}
	close(stop)
	wg.Wait()

	// whatever the interleaving, the sink is still usable and consistent
	sink.IncrCounter("runtime_counter", 1, nil)
	_, hash := flattenKey("runtime_counter", nil)
	v, ok := sink.counters.Load(hash)
	require.True(t, ok)
	assert.GreaterOrEqual(t, counterValue(t, v.(*counter)), float64(1))
}

func TestTouchNeverMovesTimeBackwards(t *testing.T) {
	var updated lastUpdate
	now := time.Now()

	require.True(t, updated.touch(now))

	// a slower emission must not make the entry look older than it is, but it
	// must still leave evidence that the entry was used
	require.True(t, updated.touch(now.Add(-time.Hour)), "an older emission is still live")
	assert.Equal(t, now.UnixNano()+1, updated.unixNano.Load())

	// which is what stops a retire decided before that update
	seen := now.UnixNano()
	assert.False(t, updated.retire(seen), "a retire decided before the update must fail")

	current, idle := updated.idleSince(time.Nanosecond, now.Add(time.Hour))
	require.True(t, idle)
	require.True(t, updated.retire(current))

	// a retired entry refuses every update, whatever its timestamp
	assert.False(t, updated.touch(now.Add(time.Hour)))
	assert.False(t, updated.touch(now.Add(-time.Hour)))
}

func TestPushSinkShutdownWaitsForTheLoop(t *testing.T) {
	var pushes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		defer func() { _ = r.Body.Close() }()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	require.NoError(t, err)

	sink, err := NewPushSinkFrom(u.Host, 5*time.Millisecond, "shutdown_test", Opts{
		Name: "shutdown_test_sink",
	})
	require.NoError(t, err)

	sink.IncrCounter("runtime_counter", 1, nil)
	require.Eventually(t, func() bool {
		return pushes.Load() > 0
	}, time.Second, 5*time.Millisecond)

	sink.Shutdown()

	// the push loop must be gone before Shutdown returns, so its push and the
	// final one cannot share the pusher
	select {
	case <-sink.doneChan:
	default:
		t.Fatal("Shutdown returned while the push loop was still running")
	}
	assert.Positive(t, pushes.Load())

	// and a second call neither blocks nor panics
	require.NotPanics(t, sink.Shutdown)
}

func TestEmissionIsReappliedWhenRetiredMidFlight(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:              "retire_midflight_sink",
		Registerer:        prometheus.NewRegistry(),
		Expiration:        time.Nanosecond,
		CounterExpiration: time.Nanosecond,
	})
	require.NoError(t, err)

	sink.IncrCounter("runtime_counter", 1, nil)
	sink.SetGauge("runtime_gauge", 1, nil)

	counterHash := mustHash(t, "runtime_counter")
	gaugeHash := mustHash(t, "runtime_gauge")

	retired, ok := sink.counters.Load(counterHash)
	require.True(t, ok)
	retiredGauge, ok := sink.gauges.Load(gaugeHash)
	require.True(t, ok)

	// the collector retires both series while an emission is in flight: the
	// entries are tombstoned but still in the map
	future := time.Now().Add(time.Hour)
	tombstoneEntry(t, &retired.(*counter).lastUpdate, future)
	tombstoneEntry(t, &retiredGauge.(*gauge).lastUpdate, future)

	sink.IncrCounter("runtime_counter", 5, nil)
	sink.SetGauge("runtime_gauge", 7, nil)

	// the value must not be left on the metric the collector discarded
	next, ok := sink.counters.Load(counterHash)
	require.True(t, ok)
	assert.NotSame(t, retired, next)
	assert.Equal(t, float64(5), counterValue(t, next.(*counter)))

	nextGauge, ok := sink.gauges.Load(gaugeHash)
	require.True(t, ok)
	assert.NotSame(t, retiredGauge, nextGauge)
	assert.Equal(t, float64(7), gaugeValue(t, nextGauge.(*gauge)))
}

// mustHash returns the series hash of an untagged metric.
func mustHash(t *testing.T, name string) string {
	t.Helper()
	_, hash := flattenKey(name, nil)
	return hash
}

// tombstoneEntry marks the entry retired without removing it from the sink,
// which is the window an emission has to detect.
func tombstoneEntry(t *testing.T, updated *lastUpdate, at time.Time) {
	t.Helper()
	seen, idle := updated.idleSince(time.Nanosecond, at)
	require.True(t, idle)
	require.True(t, updated.retire(seen))
}

// gaugeValue reads the current value of the gauge.
func gaugeValue(t *testing.T, g *gauge) float64 {
	t.Helper()
	var pb dto.Metric
	require.NoError(t, g.Write(&pb))
	return pb.GetGauge().GetValue()
}

func TestValidSeries(t *testing.T) {
	tcases := []struct {
		name   string
		key    string
		labels []metrics.Tag
		exp    bool
	}{
		{name: "plain", key: "runtime_counter", exp: true},
		{name: "utf8 name", key: "runtime{counter}", exp: true},
		{name: "utf8 label", key: "runtime_counter", labels: []metrics.Tag{{Name: "my label", Value: "v"}}, exp: true},
		{name: "empty name", key: "", exp: false},
		{name: "invalid utf8 name", key: "runtime\xff", exp: false},
		{name: "empty label name", key: "runtime_counter", labels: []metrics.Tag{{Name: "", Value: "v"}}, exp: false},
		{name: "reserved label name", key: "runtime_counter", labels: []metrics.Tag{{Name: "__reserved", Value: "v"}}, exp: false},
		{name: "invalid utf8 label value", key: "runtime_counter", labels: []metrics.Tag{{Name: "l", Value: "v\xff"}}, exp: false},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.exp, validSeries(tc.key, tc.labels))
		})
	}
}

func TestIdleSinceDoesNotOverflow(t *testing.T) {
	var updated lastUpdate
	now := time.Now()
	require.True(t, updated.touch(now))

	// an expiration near the maximum duration must not wrap the comparison
	// around and make a fresh series look idle
	_, idle := updated.idleSince(time.Duration(math.MaxInt64), now.Add(time.Second))
	assert.False(t, idle)

	_, idle = updated.idleSince(time.Second, now.Add(2*time.Second))
	assert.True(t, idle)

	// a retired entry is idle whatever the expiration
	seen, _ := updated.idleSince(time.Second, now.Add(2*time.Second))
	require.True(t, updated.retire(seen))
	seen, idle = updated.idleSince(time.Duration(math.MaxInt64), now)
	assert.True(t, idle)
	assert.Equal(t, int64(tombstone), seen)
}

func TestRetireSkipsATombstonedEntry(t *testing.T) {
	sink, err := NewSinkFrom(Opts{
		Name:              "retire_tombstoned_sink",
		Registerer:        prometheus.NewRegistry(),
		CounterExpiration: time.Second,
	})
	require.NoError(t, err)

	sink.IncrCounter("runtime_counter", 1, nil)
	hash := mustHash(t, "runtime_counter")
	v, ok := sink.counters.Load(hash)
	require.True(t, ok)

	// a concurrent scrape retired the entry but has not removed it yet
	tombstoneEntry(t, &v.(*counter).lastUpdate, time.Now().Add(time.Hour))

	// this scrape must neither collect it nor resurrect it
	assert.True(t, retire(&sink.counters, hash, v, &v.(*counter).lastUpdate, time.Second, time.Now()))
	_, ok = sink.counters.Load(hash)
	assert.False(t, ok)
	assert.Empty(t, collected(t, sink, time.Now()))
}
