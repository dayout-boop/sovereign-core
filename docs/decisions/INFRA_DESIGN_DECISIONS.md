# 인프라 아키텍처 핵심 설계 결정 (6가지 항목)

이 문서는 PCI-DSS/보안, 네트워크 타임아웃, Edge/Core 보안 경계, 비동기 메시지 큐, 테넌트 격리 모델, GitOps 전략에 대한 최신 기술 조사(2025-2026 기준)를 바탕으로 우리 플랫폼의 설계 결정을 정리합니다.

## 1. 데이터 보안 및 컴플라이언스 (PCI-DSS v4.0.1 / ISO 27001)

### 기술적 배경
PCI-DSS v4.0.1 및 ISO 27001은 개발 언어(Go, Rust 등) 자체를 규제하지 않습니다. 핵심은 **결과적 무결성**입니다. 즉, "메모리 오염 취약점을 방어했는가?"와 "데이터가 저장/전송 시 암호화되었는가?"를 평가합니다 [1]. 특히 SaaS 환경에서는 **토큰화(Tokenization)**를 통한 규제 범위(Scope) 축소가 가장 효과적인 전략으로 평가받고 있습니다 [2].

### 우리의 설계 결정
- **언어 선택의 정당성**: Go 언어는 가비지 컬렉션을 통해 메모리 오염(Buffer Overflow 등)을 원천 차단하므로, 백엔드 로직 및 금융 정산 레이어 개발에 적합하며 PCI-DSS의 보안 코딩 요건을 충족합니다.
- **토큰화 기반 Scope 축소**: 결제 카드 정보(PAN)는 우리 데이터베이스에 직접 저장하지 않고, Stripe/Toss 등 외부 PG사의 토큰화 서비스를 이용합니다. 우리 시스템은 토큰만 보관하므로 PCI-DSS 핵심 감사 대상에서 제외(Scope Reduction)됩니다.
- **데이터 암호화**: 저장 데이터(Data at Rest)는 Vultr Block Storage의 AES-256 암호화를 적용하고, 전송 데이터(Data in Transit)는 TLS 1.3을 강제합니다.

## 2. 네트워크 타임아웃 및 회복성 (Resilience)

### 기술적 배경
분산 시스템에서 인프라 간 통신 문제로 인한 '네트워크 튕김' 현상은 불가피합니다. 2025년 기준 Go 언어 진영에서는 `context.Context`를 활용한 기한(Deadline) 전파와 지수 백오프(Exponential Backoff) 재시도 패턴이 표준으로 자리 잡았습니다 [3].

### 우리의 설계 결정
- **전역 컨텍스트 전파**: 모든 외부 API 호출(PG사, LLM API 등) 및 내부 마이크로서비스 통신에 `context.WithTimeout`을 강제 적용합니다.
- **재시도 및 서킷 브레이커**: 일시적 네트워크 오류 시 지수 백오프 기반으로 재시도하며, 지속적 실패 시 서킷 브레이커를 개방하여 연쇄 장애(Cascading Failure)를 방지합니다.
- **현재 구현 상태 개선**: 기존 코드(`app.go`)의 단순 `time.Sleep` 기반 폴링을 `context` 기반 취소 및 타임아웃 처리로 전면 개편해야 합니다.

## 3. Edge/Core 2중 보안 경계 (Zero Trust Architecture)

### 기술적 배경
최신 클라우드 보안은 '경계 방어'에서 '제로 트러스트(Zero Trust)'로 진화했습니다. SASE(Secure Access Service Edge) 아키텍처를 통해 엣지(Edge)에서 트래픽을 정제하고, 코어(Core) 사설망에서 심층 검증을 수행합니다 [4].

### 우리의 설계 결정
- **Edge Simplicity (고객 접점)**: Cloudflare를 전면 배치하여 WAF, DDoS 방어, 봇 차단, 지오 라우팅(Geo-routing)을 수행합니다. 고객은 단순하고 빠른 글로벌 엣지 엔드포인트만 경험합니다.
- **Core Compliance (내부 사설망)**: Vultr 내부 K8s 클러스터는 외부 인터넷에 직접 노출되지 않습니다. Cloudflare Tunnel(Zero Trust)을 통해서만 인그레스 트래픽이 유입되며, 내부 통신은 mTLS로 암호화됩니다.

## 4. 비동기 메시지 처리 (Go Channels vs NATS JetStream)

### 기술적 배경
단일 프로세스 내 비동기 처리는 Go Channel이 효율적이나, 분산 환경(다중 파드)에서는 이벤트 유실 방지와 내결함성이 필수적입니다. NATS JetStream은 Kafka보다 가벼우면서도 At-Least-Once 전송 보장과 메시지 영속성(Persistence)을 제공하여 최신 Go 마이크로서비스 아키텍처에서 널리 쓰입니다 [5].

### 우리의 설계 결정
- **내부 경량 비동기**: 단일 노드 내 단순 상태 업데이트나 백그라운드 작업은 기존처럼 Go Channel과 Goroutine을 사용합니다.
- **분산 이벤트 버스 도입**: 결제 완료, 테넌트 프로비저닝, Scale-to-Zero 웨이크업 등 **유실되면 안 되는 핵심 도메인 이벤트**는 NATS JetStream을 도입하여 처리합니다. 현재 구현에는 메시지 큐가 누락되어 있으므로, Phase 2에서 NATS 인프라를 추가합니다.

## 5. 테넌트 데이터 격리 모델

### 기술적 배경
SaaS 데이터베이스 격리 전략은 크게 3가지(Shared DB/Shared Schema, Shared DB/Separate Schema, Separate DB)로 나뉩니다. PostgreSQL 환경에서는 Row-Level Security(RLS)를 활용한 Shared Schema 방식이 자원 효율성이 높지만, 규제 요건이 엄격한 B2B 고객은 논리적/물리적 분리를 요구합니다 [6].

### 우리의 설계 결정
- **기본 격리 (Product 1 Neon 활용)**: Neon의 Copy-on-Write 스토리지 엔진을 활용하여, 테넌트별로 별도의 논리적 브랜치(Branch) 또는 데이터베이스 인스턴스를 동적으로 프로비저닝합니다.
- **제어평면 격리**: `sovereign_core` 제어평면 자체의 메타데이터는 단일 DB 내에서 `tenant_id` 외래키와 RLS를 결합하여 논리적으로 엄격히 격리합니다.

## 6. GitOps 무결성 및 리포지토리 2원화

### 기술적 배경
2025년 GitOps 모범 사례는 애플리케이션 소스코드 리포지토리와 인프라/매니페스트 설정 리포지토리를 분리(Dual Repository Strategy)하는 것입니다. 이는 CI(빌드)와 CD(배포)의 권한을 분리하고 보안 사고를 예방합니다 [7].

### 우리의 설계 결정
- **리포지토리 2원화**: `app-repo`(비즈니스 로직, Go/Rust 코드)와 `infra-repo`(Kubernetes 매니페스트, Helm 차트, Terraform)를 물리적으로 분리합니다.
- **ArgoCD / Flux 적용**: `infra-repo`의 변경 사항만 클러스터에 반영되도록 ArgoCD를 구성하여, 개발자가 프로덕션 환경에 직접 접근하지 못하게 하는 'Pull-based' 배포 무결성을 확보합니다.

---

### References
[1] PCI Security Standards Council. (2024). PCI DSS v4.0.1. https://blog.pcisecuritystandards.org/just-published-pci-dss-v4-0-1
[2] Lorikeet Security. PCI DSS Tokenization: How to Reduce Your Compliance Scope. https://lorikeetsecurity.com/blog/pci-dss-tokenization-scope-reduction
[3] Serif Colakel. (2025). Building Resilient Go Services: Context, Graceful Shutdown, and Retry/Timeout Patterns. https://medium.com/@serifcolakel/building-resilient-go-services-context-graceful-shutdown-and-retry-timeout-patterns-041eea332162
[4] Cloudflare. Building zero trust architecture into your startup. https://developers.cloudflare.com/reference-architecture/design-guides/zero-trust-for-startups/
[5] NATS Documentation. (2025). JetStream. https://docs.nats.io/nats-concepts/jetstream
[6] PlanetScale. (2026). Approaches to tenancy in Postgres. https://planetscale.com/blog/approaches-to-tenancy-in-postgres
[7] Reddit/Kubernetes. (2025). GitOps Principles - Separate Repositories for App & Kubernetes. https://www.reddit.com/r/kubernetes/comments/1jcn002/gitops_principles_separate_repositories_for_app/
