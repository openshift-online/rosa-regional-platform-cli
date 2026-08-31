# CloudFormation Stack Creation Timing

## Overview

For each ROSA cluster, `rosactl` creates three CloudFormation stacks: **VPC**, **IAM**, and **OIDC**. Each stack creation includes a synchronous wait-for-completion phase that blocks until AWS CloudFormation transitions the stack to a terminal state (`CREATE_COMPLETE`, `UPDATE_COMPLETE`, or failure).

This document outlines the major steps, wait points, and timing characteristics that contribute to overall cluster provisioning time.

## Stack Creation Sequence

### 1. VPC Stack (`rosa-{cluster-name}-vpc`)

**Purpose**: Creates networking infrastructure (VPC, subnets, NAT gateways, route tables, internet gateway)

**Major steps**:
1. Submit CloudFormation template via `CreateStack` API (~1s)
2. **Wait for completion** using AWS SDK waiter (polls `DescribeStacks` every 30s by default)
3. Retrieve stack outputs (VPC ID, subnet IDs, security group IDs)

**Timeout**: 15 minutes (`internal/services/clustervpc/service.go:107`)

**Typical duration**: 3-5 minutes for VPC with 3 AZs, 2-3 NAT gateways

**Resources created** (~20-30 resources):
- 1 VPC
- 3-6 subnets (public + private per AZ)
- 1-3 NAT gateways (longest-running resource, ~2-3 min each)
- 1 Internet gateway
- Route tables and associations
- Security groups

**Wait characteristics**:
- NAT gateway creation is the critical path (parallel across AZs reduces wall-clock time)
- Single NAT gateway mode (`--single-nat-gateway`) can reduce cost but doesn't significantly reduce creation time

### 2. IAM Stack (`rosa-{cluster-name}-iam`)

**Purpose**: Creates IAM roles and policies for cluster components

**Major steps**:
1. Submit CloudFormation template with `CAPABILITY_IAM` and `CAPABILITY_NAMED_IAM` capabilities
2. **Wait for completion** (15 min timeout)
3. Retrieve role ARNs

**Timeout**: 15 minutes (`internal/services/clusteriam/service.go:66`)

**Typical duration**: 1-2 minutes

**Resources created** (~18 resources):
- 9 IAM roles (ingress, cloud-controller-manager, EBS CSI, image-registry, network-config, control-plane-operator, node-pool-management, worker, instance profile)
- 9 IAM policies (one per role)

**Wait characteristics**:
- IAM resources are fast to create but subject to eventual consistency
- Trust policy updates trigger ~10-15 min eventual consistency delays (see OIDC section)

**Optimization**: Can be created *after* OIDC provider exists to avoid trust policy update (see deferred-IAM flow below)

### 3. OIDC Stack (`rosa-{cluster-name}-oidc`)

**Purpose**: Creates OIDC identity provider for IRSA (IAM Roles for Service Accounts)

**Major steps**:
1. Fetch OIDC issuer TLS thumbprint (if not provided) (~1s)
2. Submit CloudFormation template
3. **Wait for completion** (5 min timeout)
4. **Update IAM stack** trust policies with real OIDC issuer domain
5. **Wait for IAM update completion** (15 min timeout)

**Timeout**: 5 minutes for OIDC creation, 15 minutes for IAM update (`internal/services/clusteroidc/service.go:77,145`)

**Typical duration**: 30 seconds (OIDC provider) + 1-2 minutes (IAM update) + **10-15 minutes eventual consistency**

**Resources created**:
- 1 OIDC identity provider

**Wait characteristics**:
- OIDC provider creation is fast (~30s)
- IAM trust policy update is fast (~1-2 min)
- **Eventual consistency delay**: IAM trust policy changes take 10-15 min to propagate globally before IRSA tokens work reliably

**Critical path**: This is the longest synchronous wait in the current implementation

## Total Creation Time

### Current Flow (VPC → IAM → OIDC)

```
VPC:  3-5 min   (wait for NAT gateways)
IAM:  1-2 min   (wait for role creation)
OIDC: 30s       (wait for provider creation)
      + 1-2 min (wait for IAM trust policy update)
      + 10-15 min (eventual consistency delay)
─────────────────────────────────────────────
Total: ~16-24 minutes (dominated by OIDC eventual consistency)
```

### Deferred-IAM Flow (VPC → OIDC → IAM)

**Optimization**: Create IAM stack *after* OIDC provider, using the real issuer domain from the start. This avoids the trust policy UPDATE and its eventual consistency delay.

```
VPC:  3-5 min   (wait for NAT gateways)
OIDC: 30s       (wait for provider creation)
IAM:  1-2 min   (wait for role creation with correct trust policies)
─────────────────────────────────────────────
Total: ~5-8 minutes (removes 10-15 min eventual consistency wait)
```

**Implementation status**: Partially supported. `GenerateClusterConfig` computes IAM role ARNs when the IAM stack doesn't exist (`internal/services/cluster/service.go:134-153`), enabling cluster creation before IAM stack exists. Full deferred-IAM workflow requires orchestration changes.

## Wait Points Summary

| Operation | Timeout | Typical Duration | Can Parallelize? |
|-----------|---------|------------------|------------------|
| VPC stack create | 15 min | 3-5 min | No (sequential) |
| IAM stack create | 15 min | 1-2 min | **Yes** (independent of VPC) |
| OIDC provider create | 5 min | 30s | No (needs IAM stack for update) |
| IAM trust policy update | 15 min | 1-2 min | No (after OIDC) |
| IAM eventual consistency | N/A | 10-15 min | No (inherent AWS delay) |

## Timing Observability

### Current State

The CLI does **not** currently report timing metrics for individual stack operations. Users see:
- Blocking CLI execution during waits
- No progress indicators beyond SDK default output
- No per-stack timing breakdown

### Proposed Enhancement

Add timing instrumentation to track and report:

1. **Per-stack creation time**:
   - Start timestamp when `CreateStack` API called
   - End timestamp when waiter completes
   - Duration calculated and logged

2. **Breakdown by phase**:
   - API submission time (~1s)
   - Wait-for-completion time (bulk of duration)
   - Output retrieval time (~1s)

3. **Output formats**:

   **CLI stdout** (human-readable):
   ```
   ✓ VPC stack created (3m 45s)
   ✓ IAM stack created (1m 12s)
   ✓ OIDC provider created (28s)
   ✓ IAM trust policies updated (1m 05s)
   
   Total infrastructure setup: 6m 30s
   Note: IAM eventual consistency may require 10-15 min for full IRSA functionality
   ```

   **Structured output** (`--output=json`):
   ```json
   {
     "stacks": {
       "vpc": {
         "stackId": "arn:aws:cloudformation:...",
         "duration": "3m45s",
         "startedAt": "2026-06-17T14:00:00Z",
         "completedAt": "2026-06-17T14:03:45Z"
       },
       "iam": {
         "stackId": "arn:aws:cloudformation:...",
         "duration": "1m12s",
         "startedAt": "2026-06-17T14:03:46Z",
         "completedAt": "2026-06-17T14:04:58Z"
       },
       "oidc": {
         "stackId": "arn:aws:cloudformation:...",
         "duration": "28s",
         "startedAt": "2026-06-17T14:04:59Z",
         "completedAt": "2026-06-17T14:05:27Z"
       }
     },
     "totalDuration": "6m30s"
   }
   ```

4. **Per-cluster metrics query**:

   Add `rosactl cluster describe --timings` to retrieve historical timing data from CloudFormation stack events:
   
   ```bash
   $ rosactl cluster describe my-cluster --timings
   
   Stack Creation Timeline:
   ├─ rosa-my-cluster-vpc
   │  Created: 2026-06-17T14:00:00Z
   │  Completed: 2026-06-17T14:03:45Z
   │  Duration: 3m 45s
   │  Status: CREATE_COMPLETE
   │
   ├─ rosa-my-cluster-iam
   │  Created: 2026-06-17T14:03:46Z
   │  Completed: 2026-06-17T14:04:58Z
   │  Duration: 1m 12s
   │  Status: CREATE_COMPLETE
   │  Last Updated: 2026-06-17T14:05:35Z (trust policy update)
   │
   └─ rosa-my-cluster-oidc
      Created: 2026-06-17T14:04:59Z
      Completed: 2026-06-17T14:05:27Z
      Duration: 28s
      Status: CREATE_COMPLETE
   
   Total: 6m 30s
   ```

### Implementation Notes

**Where to instrument**:
- `internal/aws/cloudformation/client.go`: Wrap `CreateStack`, `UpdateStack`, `DeleteStack` with timing instrumentation
- Capture start time before API call, end time after waiter completes
- Store in context or return as part of `StackOutput` struct

**Example**:
```go
type StackOutput struct {
    StackID     string
    Outputs     map[string]string
    Duration    time.Duration  // NEW
    StartedAt   time.Time      // NEW
    CompletedAt time.Time      // NEW
}
```

**Backward compatibility**:
- Timing fields are additive, no breaking changes to existing code
- CLI commands can optionally display timing data

## Optimization Opportunities

1. **Deferred IAM creation**: Create IAM stack after OIDC provider to avoid trust policy update (~10-15 min savings)
2. **Parallel VPC/OIDC**: VPC and OIDC are independent; can parallelize (reduces serial path by ~5 min)
3. **Async stack creation**: Return immediately after API submission, poll asynchronously (improves UX, doesn't reduce wall-clock time)
4. **CloudFormation change sets**: Preview changes before applying updates (reduces unnecessary update waits)

## References

- VPC service: `internal/services/clustervpc/service.go`
- IAM service: `internal/services/clusteriam/service.go`
- OIDC service: `internal/services/clusteroidc/service.go`
- CloudFormation client: `internal/aws/cloudformation/client.go`
- Stack definitions: `internal/cloudformation/templates/*.yaml`
