# Security Audit — rosa-regional-platform-cli

**Audit Date:** 2026-06-15
**Auditor:** security-audit-agent (automated)
**Scope:** Full static analysis of Go source files, Lambda handler, CLI commands, CloudFormation templates, Dockerfile
**Previous PRs:** #45 (closed), #61 (open, superseded), #69 (open, superseded)

> This PR supersedes PRs #61 and #69 (both open). It consolidates all findings from both previous audits and adds new findings. The `/ok-to-test` comment on #69 from `cdoan1` was a CI authorization comment, not a finding dismissal. No findings have been marked as non-issues by repository owners.

---

## HIGH Findings

### HIGH-1 — Lambda IAM Role Has Unrestricted Resource Scope for All IAM Operations — Privilege Escalation Path **(carry-over from #61, unresolved)**

**File:** `internal/cloudformation/templates/lambda-bootstrap.yaml` lines ~88–107

```yaml
- Sid: IAMResourceManagement
  Effect: Allow
  Action:
    - iam:CreateRole
    - iam:AttachRolePolicy
    - iam:PutRolePolicy
    - iam:DeleteRolePolicy
    - iam:CreateOpenIDConnectProvider
    - iam:CreateInstanceProfile
    - iam:AddRoleToInstanceProfile
    # ... (full list)
  Resource: '*'
```

**Risk:** The Lambda execution role can create any IAM role, attach any managed policy (including `AdministratorAccess`), and add any role to any instance profile — scoped to ALL IAM resources (`Resource: *`). There is no IAM permission boundary enforced on roles created by the Lambda. This is a direct privilege escalation path:

1. Compromised Lambda code or event input creates a new IAM role with `Resource: *`.
2. Attaches `arn:aws:iam::aws:policy/AdministratorAccess` using `iam:AttachRolePolicy` (allowed — `Resource: *`).
3. Assumes the new role.
4. Achieves full account admin access.

**Attack vectors:**
- Compromised container image (see HIGH-3): a malicious Lambda image executes this escalation sequence.
- Forged Lambda event with manipulated `ClusterName`: if CloudFormation stack operations use the cluster name in IAM resource names without validation, specially crafted names could inject unexpected IAM resource patterns.

**What to mitigate:**
- Restrict `Resource` to `arn:aws:iam::${AWS::AccountId}:role/rosa-*` and `arn:aws:iam::${AWS::AccountId}:instance-profile/rosa-*`.
- Require a permission boundary on all created roles: add `Condition: { StringEquals: { "iam:PermissionsBoundary": "arn:aws:iam::${AccountId}:policy/rosa-cluster-role-boundary" } }` to `iam:CreateRole`.
- Restrict `iam:AttachRolePolicy` to only ROSA-specific managed policies.

---

### HIGH-2 — Lambda Role Can Pass Any IAM Role to CloudFormation — Expands Attack Surface **(carry-over from #61, unresolved)**

**File:** `internal/cloudformation/templates/lambda-bootstrap.yaml` lines ~50–52

```yaml
- Sid: PassRoleForCloudFormation
  Effect: Allow
  Action: iam:PassRole
  Resource: '*'
  Condition:
    StringEquals:
      iam:PassedToService: cloudformation.amazonaws.com
```

**Risk:** The Lambda can pass **any IAM role in the account** to CloudFormation stacks it creates. If an existing over-privileged role exists (e.g., an admin role, a role used by another service), a malicious CloudFormation template triggered by the Lambda can operate with that role's full permissions. This extends the privilege escalation surface to all roles in the account.

**Attack vector:** An attacker with Lambda invocation access crafts an event that causes the Lambda to create a CloudFormation stack referencing an existing admin role. The CloudFormation stack then uses the admin role's permissions to perform arbitrary AWS operations.

**What to mitigate:** Restrict `Resource` to `arn:aws:iam::${AWS::AccountId}:role/rosa-*`. For instance profile use, add a separate `PassRoleForInstanceProfile` statement (already present in the template) and keep the two statements separate with appropriate scoping.

---

### HIGH-3 — Lambda Handler Logs Complete Event Payload Including Sensitive Infrastructure Data **(carry-over from #69, unresolved)**

**File:** `internal/lambda/handler.go` line 38

```go
fmt.Printf("Received event: %+v\n", event)
```

**Risk:** The Lambda handler prints the complete incoming event on every invocation. Lambda stdout is forwarded to CloudWatch Logs. The event contains: cluster names, OIDC issuer URLs, OIDC thumbprints, VPC CIDR ranges, subnet CIDRs, and availability zones — a complete topology map of customer cluster infrastructure.

**Attack vector:** Attacker gains `logs:GetLogEvents` on the Lambda log group (via compromised developer credentials or overly permissive IAM), reads full event payloads from every invocation, and uses the extracted data (OIDC URLs, cluster names, subnet topology) for targeted reconnaissance or to set up a rogue OIDC provider.

**What to mitigate:** Remove the `fmt.Printf` line entirely. If debugging is necessary, log only non-sensitive fields:
```go
slog.Info("received event", "action", event.Action, "cluster_name", event.ClusterName)
```

---

### HIGH-4 — SHA-1 Used for OIDC Provider Thumbprint Calculation **(carry-over from #69, unresolved)**

**File:** `internal/crypto/thumbprint.go`

```go
import "crypto/sha1"
hash := sha1.Sum(rootCert.Raw)
return hex.EncodeToString(hash[:]), nil
```

**Risk:** SHA-1 is cryptographically broken for collision resistance (SHAttered, 2017; chosen-prefix attacks demonstrated feasible). While AWS IAM currently requires SHA-1 thumbprints for OIDC providers, using SHA-1 in code creates a dangerous pattern that may be copied elsewhere, and the dependency becomes critical if AWS weakens its OIDC validation model.

**What to mitigate:** Add an explicit code comment stating this is a forced AWS limitation (not a design choice) and link to the AWS documentation. File a tracking issue to migrate when AWS supports SHA-256 thumbprints. Ensure this pattern is not replicated in other parts of the codebase.

---

### HIGH-5 — Runtime Dockerfile Uses Unpinned `:latest` UBI Minimal Image **(carry-over from #61, unresolved)**

**File:** `Dockerfile` lines ~20–22

```dockerfile
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
```

**Risk:** The `:latest` tag is a mutable pointer. The runtime image is used for the Lambda container that executes with broad AWS IAM permissions (see HIGH-1). A compromised or regressed UBI9 minimal image could introduce vulnerabilities into the runtime environment.

Compare with `aws-nuke-cf/Containerfile`, which correctly pins the UBI9 image to a specific SHA256 digest:
```dockerfile
FROM registry.access.redhat.com/ubi9/ubi@sha256:cf13fe2aba608ea76abcac5acb3fa4d88821416e7eb45e0623a62c948853ab84
```

**What to mitigate:** Pin to a SHA256 digest:
```dockerfile
FROM registry.access.redhat.com/ubi9/ubi-minimal@sha256:<hash>
```

---

### HIGH-6 — Build Stage Dockerfile Uses Unpinned `go-toolset` Image **(NEW)**

**File:** `Dockerfile` line 2

```dockerfile
FROM registry.access.redhat.com/ubi9/go-toolset:1780490457 AS builder
```

**Risk:** The build stage uses a numeric tag (`1780490457`) which appears to be a build ID, not a stable semantic version or a digest. This tag may be mutable (the registry could push a new image with the same tag). The build stage compiles the application binary — a compromise here affects the compiled output that runs in Lambda.

**What to mitigate:** Pin to a SHA256 digest to guarantee reproducibility:
```dockerfile
FROM registry.access.redhat.com/ubi9/go-toolset@sha256:<hash> AS builder
```

---

## MEDIUM Findings

### MED-1 — Lambda IAM Role Has Unrestricted EC2/Route53 Scope — Can Affect Non-ROSA Resources **(carry-over from #61, unresolved)**

**File:** `internal/cloudformation/templates/lambda-bootstrap.yaml`

```yaml
- Sid: VPCResourceManagement
  Action: [ec2:*, various VPC ops]
  Resource: '*'

- Sid: Route53ResourceManagement
  Action: [route53:CreateHostedZone, ...]
  Resource: '*'
```

**Risk:** The Lambda can modify security groups, route tables, and VPC configurations for **all** VPCs in the account. It can also create/delete Route53 hosted zones and associate any VPC with any zone — affecting DNS for non-ROSA services. A compromised Lambda invocation could disrupt networking for unrelated production workloads.

**What to mitigate:** Restrict EC2 operations to resources tagged `ManagedBy: rosactl`. Restrict Route53 operations to hosted zone patterns matching ROSA naming conventions.

---

### MED-2 — Cluster Config Output Files Written with World-Readable Permissions `0644` **(carry-over from #69, unresolved)**

**File:** `internal/commands/cluster/create.go` lines ~197, ~243

```go
if err := os.WriteFile(opts.outputFile, jsonBytes, 0644); err != nil {
```

**Risk:** The `--output-file` flag writes cluster configuration JSON (containing IAM role ARNs, VPC IDs, subnet IDs) with `0644` (world-readable). The config file itself at `internal/config/config.go` correctly uses `0600` — this inconsistency creates a privilege leak for cluster metadata.

**What to mitigate:** Change to `0600`:
```go
if err := os.WriteFile(opts.outputFile, jsonBytes, 0600); err != nil {
```

---

### MED-3 — CLI Config Directory Created with World-Readable Permissions `0755` **(carry-over from #61/#69, unresolved)**

**File:** `internal/config/config.go` lines ~38–44

```go
if err := os.MkdirAll(configDirPath, 0755); err != nil {
```

**Risk:** `~/.rosactl/` is created with world-readable, world-executable permissions. Any user on a multi-user host can list the directory contents, revealing the presence of `config.json` and any other files stored there.

**What to mitigate:** Change to `0700`:
```go
if err := os.MkdirAll(configDirPath, 0700); err != nil {
```

---

### MED-4 — URL Query Parameters Not Properly Encoded in API Calls **(carry-over from #69, unresolved)**

**File:** `internal/commands/cluster/list.go` lines ~95–98

**Risk:** Status and other filter values are interpolated into query strings using `fmt.Sprintf` without URL encoding. If a status value contains special characters (`&`, `=`, `+`), the resulting URL is malformed and could be interpreted as additional query parameters by the API server.

**What to mitigate:** Use `url.Values` to construct query strings:
```go
params := url.Values{}
params.Set("status", statusFilter)
endpoint := fmt.Sprintf("%s/api/v0/clusters?%s", baseURL, params.Encode())
```
