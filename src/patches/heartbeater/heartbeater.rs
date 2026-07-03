use std::collections::HashMap;
use std::fmt::Debug;
use std::future::Future;
use std::sync::Arc;
use std::time::{Duration, Instant};

use futures::StreamExt;
use futures::stream::FuturesUnordered;
use pageserver_api::controller_api::{NodeAvailability, SkSchedulingPolicy};
use pageserver_api::models::PageserverUtilization;
use safekeeper_api::models::SafekeeperUtilization;
use safekeeper_client::mgmt_api;
use thiserror::Error;
use tokio_util::sync::CancellationToken;
use tracing::Instrument;
use utils::id::NodeId;
use utils::logging::SecretString;

use crate::node::Node;
use crate::phi_accrual_detector::{DEFAULT_PHI_THRESHOLD, PhiAccrualDetector};
use crate::safekeeper::Safekeeper;

// [sovereign_core] Suspicion threshold at which a node is declared Offline by the
// adaptive phi-accrual detector. See phi_accrual_detector.rs for the model.
const PHI_THRESHOLD: f64 = DEFAULT_PHI_THRESHOLD;

struct HeartbeaterTask<Server, State> {
    receiver: tokio::sync::mpsc::UnboundedReceiver<HeartbeatRequest<Server, State>>,
    cancel: CancellationToken,

    state: HashMap<NodeId, State>,

    // [sovereign_core] Per-node adaptive failure detectors. These learn each
    // node's heartbeat rhythm and replace the single fixed `max_offline_interval`
    // comparison when deciding to mark a node Offline. `max_offline_interval` is
    // retained and passed to each detector as the cold-start fallback grace
    // period, so behaviour before warm-up is identical to upstream.
    detectors: HashMap<NodeId, PhiAccrualDetector>,

    max_offline_interval: Duration,
    max_warming_up_interval: Duration,
    http_client: reqwest::Client,
    jwt_token: Option<String>,
}

#[derive(Debug, Clone)]
pub(crate) enum PageserverState {
    Available {
        // [sovereign_core] Retained for observability/debug output. Failure
        // detection no longer reads this directly; the adaptive phi-accrual
        // detector (see phi_accrual_detector.rs) tracks heartbeat timing instead.
        #[allow(dead_code)]
        last_seen_at: Instant,
        utilization: PageserverUtilization,
    },
    WarmingUp {
        started_at: Instant,
    },
    Offline,
}

#[derive(Debug, Clone)]
pub(crate) enum SafekeeperState {
    Available {
        last_seen_at: Instant,
        utilization: SafekeeperUtilization,
    },
    Offline,
}

#[derive(Debug)]
pub(crate) struct AvailablityDeltas<State>(pub Vec<(NodeId, State)>);

#[derive(Debug, Error)]
pub(crate) enum HeartbeaterError {
    #[error("Cancelled")]
    Cancel,
}

struct HeartbeatRequest<Server, State> {
    servers: Arc<HashMap<NodeId, Server>>,
    reply: tokio::sync::oneshot::Sender<Result<AvailablityDeltas<State>, HeartbeaterError>>,
}

pub(crate) struct Heartbeater<Server, State> {
    sender: tokio::sync::mpsc::UnboundedSender<HeartbeatRequest<Server, State>>,
}

#[allow(private_bounds)]
impl<Server: Send + Sync + 'static, State: Debug + Send + 'static> Heartbeater<Server, State>
where
    HeartbeaterTask<Server, State>: HeartBeat<Server, State>,
{
    pub(crate) fn new(
        http_client: reqwest::Client,
        jwt_token: Option<String>,
        max_offline_interval: Duration,
        max_warming_up_interval: Duration,
        cancel: CancellationToken,
    ) -> Self {
        let (sender, receiver) =
            tokio::sync::mpsc::unbounded_channel::<HeartbeatRequest<Server, State>>();
        let mut heartbeater = HeartbeaterTask::new(
            receiver,
            http_client,
            jwt_token,
            max_offline_interval,
            max_warming_up_interval,
            cancel,
        );
        tokio::task::spawn(async move { heartbeater.run().await });

        Self { sender }
    }

    pub(crate) async fn heartbeat(
        &self,
        servers: Arc<HashMap<NodeId, Server>>,
    ) -> Result<AvailablityDeltas<State>, HeartbeaterError> {
        let (sender, receiver) = tokio::sync::oneshot::channel();
        self.sender
            .send(HeartbeatRequest {
                servers,
                reply: sender,
            })
            .map_err(|_| HeartbeaterError::Cancel)?;

        receiver
            .await
            .map_err(|_| HeartbeaterError::Cancel)
            .and_then(|x| x)
    }
}

impl<Server, State: Debug> HeartbeaterTask<Server, State>
where
    HeartbeaterTask<Server, State>: HeartBeat<Server, State>,
{
    fn new(
        receiver: tokio::sync::mpsc::UnboundedReceiver<HeartbeatRequest<Server, State>>,
        http_client: reqwest::Client,
        jwt_token: Option<String>,
        max_offline_interval: Duration,
        max_warming_up_interval: Duration,
        cancel: CancellationToken,
    ) -> Self {
        Self {
            receiver,
            cancel,
            state: HashMap::new(),
            // [sovereign_core] detectors are created lazily per node on first heartbeat.
            detectors: HashMap::new(),
            max_offline_interval,
            max_warming_up_interval,
            http_client,
            jwt_token,
        }
    }
    async fn run(&mut self) {
        loop {
            tokio::select! {
                request = self.receiver.recv() => {
                    match request {
                        Some(req) => {
                            if req.reply.is_closed() {
                                // Prevent a possibly infinite buildup of the receiver channel, if requests arrive faster than we can handle them
                                continue;
                            }
                            let res = self.heartbeat(req.servers).await;
                            // Ignore the return value in order to not panic if the heartbeat function's future was cancelled
                            _ = req.reply.send(res);
                        },
                        None => { return; }
                    }
                },
                _ = self.cancel.cancelled() => return
            }
        }
    }
}

pub(crate) trait HeartBeat<Server, State> {
    fn heartbeat(
        &mut self,
        pageservers: Arc<HashMap<NodeId, Server>>,
    ) -> impl Future<Output = Result<AvailablityDeltas<State>, HeartbeaterError>> + Send;
}

impl HeartBeat<Node, PageserverState> for HeartbeaterTask<Node, PageserverState> {
    async fn heartbeat(
        &mut self,
        pageservers: Arc<HashMap<NodeId, Node>>,
    ) -> Result<AvailablityDeltas<PageserverState>, HeartbeaterError> {
        let mut new_state = HashMap::new();

        let mut heartbeat_futs = FuturesUnordered::new();
        for (node_id, node) in &*pageservers {
            heartbeat_futs.push({
                let http_client = self.http_client.clone();
                let jwt_token = self.jwt_token.clone();
                let cancel = self.cancel.clone();

                // Clone the node and mark it as available such that the request
                // goes through to the pageserver even when the node is marked offline.
                // This doesn't impact the availability observed by [`crate::service::Service`].
                let mut node_clone = node.clone();
                node_clone
                    .set_availability(NodeAvailability::Active(PageserverUtilization::full()));

                async move {
                    let response = node_clone
                        .with_client_retries(
                            |client| async move { client.get_utilization().await },
                            &http_client,
                            &jwt_token,
                            3,
                            3,
                            Duration::from_secs(1),
                            &cancel,
                        )
                        .await;

                    let response = match response {
                        Some(r) => r,
                        None => {
                            // This indicates cancellation of the request.
                            // We ignore the node in this case.
                            return None;
                        }
                    };

                    let status = if let Ok(utilization) = response {
                        PageserverState::Available {
                            last_seen_at: Instant::now(),
                            utilization,
                        }
                    } else if let NodeAvailability::WarmingUp(last_seen_at) =
                        node.get_availability()
                    {
                        PageserverState::WarmingUp {
                            started_at: *last_seen_at,
                        }
                    } else {
                        PageserverState::Offline
                    };

                    Some((*node_id, status))
                }
                .instrument(tracing::info_span!("heartbeat_ps", %node_id))
            });
        }

        loop {
            let maybe_status = tokio::select! {
                next = heartbeat_futs.next() => {
                    match next {
                        Some(result) => result,
                        None => { break; }
                    }
                },
                _ = self.cancel.cancelled() => { return Err(HeartbeaterError::Cancel); }
            };

            if let Some((node_id, status)) = maybe_status {
                new_state.insert(node_id, status);
            }
        }

        let mut warming_up = 0;
        let mut offline = 0;
        for state in new_state.values() {
            match state {
                PageserverState::WarmingUp { .. } => {
                    warming_up += 1;
                }
                PageserverState::Offline => offline += 1,
                PageserverState::Available { .. } => {}
            }
        }

        tracing::info!(
            "Heartbeat round complete for {} nodes, {} warming-up, {} offline",
            new_state.len(),
            warming_up,
            offline
        );

        let mut deltas = Vec::new();
        let now = Instant::now();
        for (node_id, ps_state) in new_state.iter_mut() {
            use std::collections::hash_map::Entry::*;

            // [sovereign_core] Feed the adaptive detector for this node. A
            // successful heartbeat this round records an inter-arrival sample so
            // the detector learns the node's rhythm; the detector is created
            // lazily with `max_offline_interval` as its cold-start fallback grace.
            let detector = self
                .detectors
                .entry(*node_id)
                .or_insert_with(|| PhiAccrualDetector::new(self.max_offline_interval));
            if let PageserverState::Available { .. } = ps_state {
                detector.record_heartbeat(now);
            }

            let entry = self.state.entry(*node_id);

            let mut needs_update = false;
            match entry {
                Occupied(ref occ) => match (occ.get(), &ps_state) {
                    (PageserverState::Offline, PageserverState::Offline) => {}
                    (PageserverState::Available { .. }, PageserverState::Offline) => {
                        // [sovereign_core] Replace the fixed `max_offline_interval`
                        // grace with the adaptive phi-accrual suspicion level. The
                        // node is declared Offline only once suspicion crosses
                        // PHI_THRESHOLD. Before enough samples are learned the
                        // detector falls back to the old fixed grace, so early
                        // behaviour is unchanged.
                        if !detector.is_available(now, PHI_THRESHOLD) {
                            deltas.push((*node_id, ps_state.clone()));
                            needs_update = true;
                        }
                    }
                    (_, PageserverState::WarmingUp { started_at }) => {
                        if now - *started_at >= self.max_warming_up_interval {
                            *ps_state = PageserverState::Offline;
                        }

                        deltas.push((*node_id, ps_state.clone()));
                        needs_update = true;
                    }
                    _ => {
                        deltas.push((*node_id, ps_state.clone()));
                        needs_update = true;
                    }
                },
                Vacant(_) => {
                    // This is a new node. Don't generate a delta for it.
                    deltas.push((*node_id, ps_state.clone()));
                }
            }

            match entry {
                Occupied(mut occ) if needs_update => {
                    (*occ.get_mut()) = ps_state.clone();
                }
                Vacant(vac) => {
                    vac.insert(ps_state.clone());
                }
                _ => {}
            }
        }

        // [sovereign_core] Drop detectors for nodes not present this round so the
        // per-node detector map cannot grow unbounded as nodes are decommissioned.
        self.detectors.retain(|id, _| new_state.contains_key(id));

        Ok(AvailablityDeltas(deltas))
    }
}

impl HeartBeat<Safekeeper, SafekeeperState> for HeartbeaterTask<Safekeeper, SafekeeperState> {
    async fn heartbeat(
        &mut self,
        safekeepers: Arc<HashMap<NodeId, Safekeeper>>,
    ) -> Result<AvailablityDeltas<SafekeeperState>, HeartbeaterError> {
        let mut new_state = HashMap::new();

        let mut heartbeat_futs = FuturesUnordered::new();
        for (node_id, sk) in &*safekeepers {
            if sk.scheduling_policy() == SkSchedulingPolicy::Decomissioned {
                continue;
            }
            heartbeat_futs.push({
                let http_client = self.http_client.clone();
                let jwt_token = self
                    .jwt_token
                    .as_ref()
                    .map(|t| SecretString::from(t.to_owned()));
                let cancel = self.cancel.clone();

                async move {
                    let response = sk
                        .with_client_retries(
                            |client| async move { client.get_utilization().await },
                            &http_client,
                            &jwt_token,
                            3,
                            3,
                            Duration::from_secs(1),
                            &cancel,
                        )
                        .await;

                    let status = match response {
                        Ok(utilization) => SafekeeperState::Available {
                            last_seen_at: Instant::now(),
                            utilization,
                        },
                        Err(mgmt_api::Error::Cancelled) => {
                            // This indicates cancellation of the request.
                            // We ignore the node in this case.
                            return None;
                        }
                        Err(e) => {
                            tracing::info!(
                                "Marking safekeeper {} at as offline: {e}",
                                sk.base_url()
                            );
                            SafekeeperState::Offline
                        }
                    };

                    Some((*node_id, status))
                }
                .instrument(tracing::info_span!("heartbeat_sk", %node_id))
            });
        }

        loop {
            let maybe_status = tokio::select! {
                next = heartbeat_futs.next() => {
                    match next {
                        Some(result) => result,
                        None => { break; }
                    }
                },
                _ = self.cancel.cancelled() => { return Err(HeartbeaterError::Cancel); }
            };

            if let Some((node_id, status)) = maybe_status {
                new_state.insert(node_id, status);
            }
        }

        let mut offline = 0;
        for state in new_state.values() {
            match state {
                SafekeeperState::Offline => offline += 1,
                SafekeeperState::Available { .. } => {}
            }
        }

        tracing::info!(
            "Heartbeat round complete for {} safekeepers, {} offline",
            new_state.len(),
            offline
        );

        let mut deltas = Vec::new();
        let now = Instant::now();
        for (node_id, sk_state) in new_state.iter_mut() {
            use std::collections::hash_map::Entry::*;

            // [sovereign_core] Safekeepers use the same adaptive detector as
            // pageservers so quorum-affecting failures are surfaced promptly on
            // steady links while jittery links keep their extra slack.
            let detector = self
                .detectors
                .entry(*node_id)
                .or_insert_with(|| PhiAccrualDetector::new(self.max_offline_interval));
            if let SafekeeperState::Available { .. } = sk_state {
                detector.record_heartbeat(now);
            }

            let entry = self.state.entry(*node_id);

            let mut needs_update = false;
            match entry {
                Occupied(ref occ) => match (occ.get(), &sk_state) {
                    (SafekeeperState::Offline, SafekeeperState::Offline) => {}
                    (SafekeeperState::Available { .. }, SafekeeperState::Offline) => {
                        if !detector.is_available(now, PHI_THRESHOLD) {
                            deltas.push((*node_id, sk_state.clone()));
                            needs_update = true;
                        }
                    }
                    _ => {
                        deltas.push((*node_id, sk_state.clone()));
                        needs_update = true;
                    }
                },
                Vacant(_) => {
                    // This is a new node. Don't generate a delta for it.
                    deltas.push((*node_id, sk_state.clone()));
                }
            }

            match entry {
                Occupied(mut occ) if needs_update => {
                    (*occ.get_mut()) = sk_state.clone();
                }
                Vacant(vac) => {
                    vac.insert(sk_state.clone());
                }
                _ => {}
            }
        }

        // [sovereign_core] Same detector cleanup as the pageserver path.
        self.detectors.retain(|id, _| new_state.contains_key(id));

        Ok(AvailablityDeltas(deltas))
    }
}
