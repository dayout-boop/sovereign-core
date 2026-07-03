# 물리 격리 환경 2노드 HA 백업 및 자동 복원 설계

이 문서는 Sovereign Core의 물리 격리(Air-gapped) 및 네트워크 차단 환경에서 상시 서버 2대 간의 고가용성(HA) 백업 아키텍처와 장애 시 자동 복원 파이프라인에 대한 설계 결정을 정의합니다.

## 1. 아키텍처 개요 및 제약 사항

물리 격리 및 네트워크 차단 환경에서는 외부 클라우드 스토리지(S3 등)나 매니지드 서비스에 의존할 수 없습니다. 상시 운영되는 2대의 노드(Node A: Active, Node B: Standby)만으로 데이터 무결성을 보장하고, 단일 노드 장애 시 자동 페일오버(Failover)를 수행해야 합니다.

가장 큰 제약은 **Split-Brain(스플릿 브레인)** 현상입니다. 2노드 클러스터에서는 네트워크 단절 시 양쪽 노드가 모두 자신이 Active라고 판단하여 데이터를 동시에 쓰게 되어 심각한 데이터 손상이 발생할 수 있습니다.

## 2. Split-Brain 방지 설계 (Witness Node 패턴)

전통적인 합의 알고리즘(Raft, Paxos)은 정족수(Quorum, N/2 + 1)를 요구하므로 2노드에서는 장애 내성을 가질 수 없습니다. 이를 해결하기 위해 컴퓨팅 자원을 거의 소모하지 않는 **제3의 경량 노드(Witness Node)**를 도입합니다.

**설계 결정:**
- **구조**: Node A (Data+Compute), Node B (Data+Compute), Witness Node (Vote only).
- **역할**: Witness Node는 데이터를 저장하지 않으며, 오직 네트워크 단절 시 어느 노드가 Active가 될지 결정하는 투표권(Tie-breaker)만 행사합니다.
- **STONITH (Shoot The Other Node In The Head)**: Active 노드 전환 시, 기존 Active 노드의 전원을 차단하거나 네트워크 포트를 비활성화(Fencing)하여 데이터 오염을 원천 차단합니다.

## 3. 데이터 백업 및 복제 아키텍처

데이터베이스(PostgreSQL/Neon 기반)와 쿠버네티스 상태(etcd, PV)를 각각 다른 방식으로 복제합니다.

### 3-1. 데이터베이스 복제 (WAL 스트리밍)

Neon 엔진의 분리된 스토리지 아키텍처(Lakebase)를 활용하여 컴퓨팅과 스토리지를 분리 복제합니다.

**설계 결정:**
- **동기식 WAL 스트리밍**: Node A의 Neon Compute에서 발생한 WAL(Write-Ahead Log) 레코드를 Node A와 Node B의 Safekeeper 노드로 동시 스트리밍합니다.
- **Safekeeper Quorum**: 최소 2개의 Safekeeper(Node A, Node B) 중 과반수의 응답을 받아야 트랜잭션이 커밋됩니다. (Witness 노드에 경량 Safekeeper를 배치하여 3노드 Quorum 구성 가능).
- **Pageserver 복제**: WAL 데이터를 기반으로 Pageserver가 백그라운드에서 데이터를 물질화(Materialize)하여 Node B의 로컬 스토리지에 복제본을 유지합니다.

### 3-2. 쿠버네티스 상태 및 볼륨 백업 (Velero + 로컬 MinIO)

네트워크 차단 환경이므로 외부 S3 대신 클러스터 내부에 S3 호환 스토리지를 구축합니다.

**설계 결정:**
- **로컬 객체 스토리지**: Node B에 MinIO를 배포하여 S3 호환 백업 저장소로 사용합니다.
- **Velero 스케줄링**: Velero를 사용하여 매시간 etcd 스냅샷과 Persistent Volume(CSI/Rook-Ceph 기반) 스냅샷을 생성하고 Node B의 MinIO로 전송합니다.
- **Rook-Ceph 비동기 복제**: 중요도가 매우 높은 볼륨은 Rook-Ceph의 RBD 미러링을 통해 Node A에서 Node B로 블록 레벨 비동기 복제를 수행합니다.

## 4. 자동 복원(Auto-Recovery) 파이프라인

장애 감지부터 서비스 정상화까지의 모든 과정을 자동화 봇이 처리합니다.

### 4-1. 장애 감지 및 페일오버 흐름

1. **장애 감지**: NATS 기반의 `HeartbeatPort`가 Node A의 응답 누락을 감지합니다 (Phi-accrual detector 활용).
2. **Quorum 확인**: Node B와 Witness Node가 통신하여 Node A의 장애를 확정합니다.
3. **Fencing (STONITH)**: Node B의 `FailoverPort`가 Node A의 네트워크 인터페이스를 차단합니다.
4. **Active 승격**: Node B의 Neon Compute가 시작되고, 로컬 Safekeeper와 Pageserver를 바라보도록 라우팅이 변경됩니다.
5. **트래픽 전환**: 클러스터 내부의 VIP(Virtual IP) 또는 로드밸런서가 Node B를 향하도록 ARP/BGP 업데이트를 수행합니다.

### 4-2. 데이터 복원 흐름 (Node A 복구 시)

장애가 발생했던 Node A가 다시 켜졌을 때의 복원 절차입니다.

1. **상태 동기화**: Node A는 즉시 Standby 모드로 부팅됩니다.
2. **WAL Catch-up**: Node A의 Safekeeper가 Node B로부터 누락된 WAL 레코드를 스트리밍 받아 동기화합니다.
3. **Velero 복원**: 필요한 경우 Velero가 Node B의 MinIO에서 쿠버네티스 리소스(Deployment, ConfigMap 등)를 Node A로 복원합니다.
4. **HA 복구 완료**: 동기화가 완료되면 다시 정상적인 HA(Active-Standby) 상태로 돌아갑니다.

## 5. 포트 및 봇 설계 (코드 구현 대상)

이 아키텍처를 제어평면에 통합하기 위해 다음 컴포넌트를 구현합니다.

- **HeartbeatPort**: 노드 간 상태를 모니터링하고 Phi-accrual 장애 감지 로직을 수행.
- **FailoverPort**: Quorum 기반 리더 선출, STONITH 펜싱 실행, 트래픽 라우팅 전환.
- **BackupPort**: Velero API 연동 및 로컬 MinIO 백업 스케줄링 관리.
- **FailoverBot**: NATS 이벤트를 수신하여 자동 페일오버 파이프라인을 오케스트레이션하는 백그라운드 워커.

---
*이 설계는 `MASTER_ARCHITECTURE_V1.md`의 인프라 제약 사항을 준수하며, 사용자 확인 후 코드로 구현됩니다.*
