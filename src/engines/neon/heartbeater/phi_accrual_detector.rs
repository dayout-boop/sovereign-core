// [sovereign_core] Phi-accrual adaptive failure detector.
//
// # Why this module exists
//
// Upstream Neon's `HeartbeaterTask` marks a pageserver / safekeeper `Offline`
// using a single fixed grace period, `max_offline_interval` (30s in the default
// config; see `storage_controller/src/service.rs`). Concretely, the decision is:
//
//     if now - last_seen_at >= self.max_offline_interval { -> Offline }
//
// A fixed threshold forces a hard tradeoff:
//   * Set it low  -> fast failover, but false positives on transient network
//                    jitter/GC pauses, which trigger needless (and costly) shard
//                    migrations.
//   * Set it high -> few false positives, but every real crash costs the full
//                    30s of blackout before failover starts.
//
// Because 0.1s of blackout maps directly to lost request capacity (and therefore
// billed credits) at 10,000 concurrent users, neither static choice is good
// enough. The phi-accrual detector (Hayashibara et al., 2004, "The Φ Accrual
// Failure Detector") replaces the fixed threshold with a *suspicion level* `phi`
// derived from the *observed distribution of heartbeat inter-arrival times* for
// each node. A node that has historically been steady is suspected quickly when
// it goes quiet; a node on a jittery link is given proportionally more slack.
// The effective timeout therefore adapts per-node and per-network-condition
// instead of being a single global constant.
//
// # Model
//
// We record the inter-arrival interval between successful heartbeats into a
// bounded sliding window and model them as a normal distribution N(mean, stddev).
// Given the elapsed time `t` since the last heartbeat, the suspicion value is:
//
//     phi(t) = -log10( P(later than t) )
//            = -log10( 1 - F(t) )
//
// where F is the CDF of the fitted normal distribution. phi grows without bound
// as `t` exceeds the historically-normal interval. A node is considered failed
// once `phi >= threshold` (a threshold of 8.0 corresponds to a ~1e-8 probability
// that the node is actually alive, i.e. a very confident suspicion).
//
// # Safety / correctness notes
//
//   * This detector only *decides when to suspect*; it does not itself perform
//     any state transition or migration. The caller (`heartbeater.rs`) keeps full
//     control and only substitutes `is_available()` for the old
//     `now - last_seen_at >= max_offline_interval` comparison.
//   * Until enough samples are collected (`MIN_SAMPLES`), we fall back to the
//     supplied static grace period so startup behaviour is identical to upstream.
//   * A configurable floor (`min_std_dev_ms`) prevents an unrealistically small
//     stddev (from a perfectly regular link) from making the detector hair-trigger.
//   * All arithmetic is on `f64` milliseconds; no unsafe code, no external deps.

use std::collections::VecDeque;
use std::time::{Duration, Instant};

/// Default suspicion threshold. phi == 8.0 => ~1e-8 probability the node is alive.
/// Higher = more conservative (fewer false positives, slower detection).
pub(crate) const DEFAULT_PHI_THRESHOLD: f64 = 8.0;

/// Number of heartbeat intervals kept in the sliding window.
const MAX_SAMPLES: usize = 200;

/// Minimum samples required before phi is trusted; below this we defer to the
/// static grace period so cold-start behaviour matches upstream.
const MIN_SAMPLES: usize = 8;

/// Floor on the estimated standard deviation to avoid a hair-trigger detector on
/// an unrealistically regular link.
const DEFAULT_MIN_STD_DEV_MS: f64 = 50.0;

/// Per-node adaptive failure detector based on the phi-accrual algorithm.
#[derive(Debug, Clone)]
pub(crate) struct PhiAccrualDetector {
    /// Sliding window of observed inter-arrival intervals (milliseconds).
    intervals: VecDeque<f64>,
    /// Timestamp of the most recent successful heartbeat, if any.
    last_heartbeat: Option<Instant>,
    /// Running sum of samples (for O(1) mean).
    sum: f64,
    /// Running sum of squares of samples (for O(1) variance).
    sum_sq: f64,
    /// Lower bound on estimated stddev, milliseconds.
    min_std_dev_ms: f64,
    /// Static grace period used until enough samples are collected. This is the
    /// old `max_offline_interval`, so behaviour before warm-up is unchanged.
    fallback_grace: Duration,
}

impl PhiAccrualDetector {
    /// Create a detector that falls back to `fallback_grace` until it has learned
    /// the node's heartbeat rhythm.
    pub(crate) fn new(fallback_grace: Duration) -> Self {
        Self {
            intervals: VecDeque::with_capacity(MAX_SAMPLES),
            last_heartbeat: None,
            sum: 0.0,
            sum_sq: 0.0,
            min_std_dev_ms: DEFAULT_MIN_STD_DEV_MS,
            fallback_grace,
        }
    }

    /// Record a successful heartbeat observed at `now`. The interval since the
    /// previous heartbeat is added to the sliding window.
    pub(crate) fn record_heartbeat(&mut self, now: Instant) {
        if let Some(prev) = self.last_heartbeat {
            let interval_ms = now.saturating_duration_since(prev).as_secs_f64() * 1000.0;
            self.push_interval(interval_ms);
        }
        self.last_heartbeat = Some(now);
    }

    fn push_interval(&mut self, interval_ms: f64) {
        if self.intervals.len() == MAX_SAMPLES {
            if let Some(old) = self.intervals.pop_front() {
                self.sum -= old;
                self.sum_sq -= old * old;
            }
        }
        self.intervals.push_back(interval_ms);
        self.sum += interval_ms;
        self.sum_sq += interval_ms * interval_ms;
    }

    fn mean(&self) -> f64 {
        self.sum / self.intervals.len() as f64
    }

    fn std_dev(&self) -> f64 {
        let n = self.intervals.len() as f64;
        let mean = self.mean();
        let variance = (self.sum_sq / n) - (mean * mean);
        variance.max(0.0).sqrt().max(self.min_std_dev_ms)
    }

    /// Current suspicion value phi given `now`. Returns 0.0 when there is no
    /// baseline yet (no heartbeat recorded).
    pub(crate) fn phi(&self, now: Instant) -> f64 {
        let last = match self.last_heartbeat {
            Some(t) => t,
            None => return 0.0,
        };

        let elapsed_ms = now.saturating_duration_since(last).as_secs_f64() * 1000.0;

        // Not enough history to trust the distribution: emulate the static grace
        // period as a step function so cold-start behaviour matches upstream.
        if self.intervals.len() < MIN_SAMPLES {
            let grace_ms = self.fallback_grace.as_secs_f64() * 1000.0;
            return if elapsed_ms >= grace_ms {
                f64::INFINITY
            } else {
                0.0
            };
        }

        let mean = self.mean();
        let std_dev = self.std_dev();

        // phi = -log10(P(elapsed > t)) where the interval ~ N(mean, std_dev).
        // P(later) = 1 - CDF(elapsed). We compute the upper-tail directly for
        // numerical stability using the complementary error function surrogate.
        let p_later = Self::p_later(elapsed_ms, mean, std_dev);
        // Clamp to avoid -log10(0) = +inf blowing up before threshold comparison.
        let p_later = p_later.max(f64::MIN_POSITIVE);
        -p_later.log10()
    }

    /// Upper-tail probability P(X > x) for X ~ N(mean, std_dev), computed via a
    /// logistic approximation of the normal CDF (Hayashibara et al. use the same
    /// approximation in the original phi-accrual paper). Accurate to well within
    /// the precision needed for a suspicion threshold.
    fn p_later(x: f64, mean: f64, std_dev: f64) -> f64 {
        let y = (x - mean) / std_dev;
        // Logistic approximation of the standard-normal upper tail.
        let e = (-y * (1.5976 + 0.070566 * y * y)).exp();
        if x > mean {
            e / (1.0 + e)
        } else {
            1.0 - 1.0 / (1.0 + e)
        }
    }

    /// True if the node should still be considered available, i.e. suspicion has
    /// not yet reached `threshold`.
    pub(crate) fn is_available(&self, now: Instant, threshold: f64) -> bool {
        self.phi(now) < threshold
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn stable_detector(interval_ms: u64, count: usize) -> (PhiAccrualDetector, Instant) {
        let mut d = PhiAccrualDetector::new(Duration::from_secs(30));
        let mut t = Instant::now();
        d.record_heartbeat(t);
        for _ in 0..count {
            t += Duration::from_millis(interval_ms);
            d.record_heartbeat(t);
        }
        (d, t)
    }

    #[test]
    fn no_history_returns_zero_phi() {
        let d = PhiAccrualDetector::new(Duration::from_secs(30));
        assert_eq!(d.phi(Instant::now()), 0.0);
    }

    #[test]
    fn cold_start_falls_back_to_static_grace() {
        // Fewer than MIN_SAMPLES intervals: behaves like the old fixed threshold.
        let mut d = PhiAccrualDetector::new(Duration::from_secs(30));
        let t0 = Instant::now();
        d.record_heartbeat(t0);
        // Just before grace: available.
        assert!(d.is_available(t0 + Duration::from_secs(29), DEFAULT_PHI_THRESHOLD));
        // After grace: suspected.
        assert!(!d.is_available(t0 + Duration::from_secs(31), DEFAULT_PHI_THRESHOLD));
    }

    #[test]
    fn steady_node_recently_seen_is_available() {
        // 1s heartbeats, long steady history.
        let (d, t) = stable_detector(1000, 50);
        // Right on schedule -> low suspicion.
        assert!(d.is_available(t + Duration::from_millis(1000), DEFAULT_PHI_THRESHOLD));
    }

    #[test]
    fn steady_node_silent_is_suspected_faster_than_static_grace() {
        // A node that reliably beats every 1s should be suspected well before the
        // old fixed 30s grace once it goes silent for several seconds.
        let (d, t) = stable_detector(1000, 100);
        let phi_at_6s = d.phi(t + Duration::from_secs(6));
        assert!(
            phi_at_6s >= DEFAULT_PHI_THRESHOLD,
            "expected suspicion by ~6s for a steady 1s-beat node, phi={phi_at_6s}"
        );
    }

    #[test]
    fn jittery_node_gets_more_slack() {
        // Build a jittery history: alternating 500ms / 3000ms intervals.
        let mut d = PhiAccrualDetector::new(Duration::from_secs(30));
        let mut t = Instant::now();
        d.record_heartbeat(t);
        for i in 0..100 {
            let step = if i % 2 == 0 { 500 } else { 3000 };
            t += Duration::from_millis(step);
            d.record_heartbeat(t);
        }
        // A jittery node should NOT be suspected as fast as the steady one: at 6s
        // its phi should be lower than the steady node's phi at the same elapsed.
        let (steady, ts) = stable_detector(1000, 100);
        let phi_jitter = d.phi(t + Duration::from_secs(6));
        let phi_steady = steady.phi(ts + Duration::from_secs(6));
        assert!(
            phi_jitter < phi_steady,
            "jittery node should be given more slack: jitter={phi_jitter} steady={phi_steady}"
        );
    }
}
