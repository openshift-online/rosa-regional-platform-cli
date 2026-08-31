# Deferred-IAM Flow Implementation

## Goal

Reduce cluster provisioning time from **~16-24 minutes** to **~5-8 minutes** by eliminating the IAM trust policy update and its 10-15 minute eventual consistency delay.

## Current Flow (16-24 min)

```
1. VPC stack create           → 3-5 min
2. IAM stack create           → 1-2 min  (with PENDING trust policies)
3. OIDC provider create       → 30s
4. IAM trust policy UPDATE    → 1-2 min  (replace PENDING with real issuer)
5. IAM eventual consistency   → 10-15 min (AWS global propagation)
```

**Problem**: Step 4 triggers eventual consistency delay because updating trust policies on existing roles requires global IAM propagation.

## Deferred-IAM Flow (5-8 min)

```
1. VPC stack create           → 3-5 min
2. OIDC provider create       → 30s
3. IAM stack create           → 1-2 min  (with correct trust policies from start)
```

**Key insight**: Create IAM roles with the final OIDC issuer domain from the start — no UPDATE needed, no eventual consistency delay.

## Implementation Strategy

### Phase 1: Add Orchestration Command

Create a new `rosactl cluster provision` command that orchestrates VPC → OIDC → IAM in the optimal order.

**Why**: 
- Existing commands (`cluster-vpc create`, `cluster-iam create`, `cluster-oidc create`) are single-purpose and should remain independent
- Users need both granular control (current commands) and optimized workflow (new command)
- Preserves backward compatibility

### Phase 2: Add Timing Instrumentation

Capture and report timing metrics for each stack operation.

### Phase 3: Update Documentation

Update guides to recommend the new workflow.

---

## Detailed Implementation

### 1. Create `internal/services/provision/service.go`

New service layer for orchestrating the deferred-IAM flow:

```go
package provision

import (
    "context"
    "fmt"
    "time"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/openshift-online/rosa-regional-platform-cli/internal/services/clusteriam"
    "github.com/openshift-online/rosa-regional-platform-cli/internal/services/clusteroidc"
    "github.com/openshift-online/rosa-regional-platform-cli/internal/services/clustervpc"
)

// ProvisionRequest contains parameters for provisioning cluster infrastructure
type ProvisionRequest struct {
    ClusterName        string
    Region             string
    
    // VPC parameters
    VpcCidr            string
    PublicSubnetCidrs  []string
    PrivateSubnetCidrs []string
    AvailabilityZones  []string
    SingleNatGateway   bool
    
    // OIDC parameters
    OIDCIssuerURL      string
    OIDCThumbprint     string // optional
    
    AWSConfig          aws.Config
}

// ProvisionResponse contains outputs from infrastructure provisioning
type ProvisionResponse struct {
    VPC  *VPCResult
    OIDC *OIDCResult
    IAM  *IAMResult
    
    TotalDuration time.Duration
}

type VPCResult struct {
    StackID      string
    Outputs      map[string]string
    Duration     time.Duration
    StartedAt    time.Time
    CompletedAt  time.Time
}

type OIDCResult struct {
    StackID      string
    Outputs      map[string]string
    Duration     time.Duration
    StartedAt    time.Time
    CompletedAt  time.Time
}

type IAMResult struct {
    StackID      string
    Outputs      map[string]string
    Duration     time.Duration
    StartedAt    time.Time
    CompletedAt  time.Time
}

// ProvisionInfrastructure provisions VPC, OIDC, and IAM in the optimal order
func ProvisionInfrastructure(ctx context.Context, req *ProvisionRequest) (*ProvisionResponse, error) {
    totalStart := time.Now()
    resp := &ProvisionResponse{}
    
    // Step 1: Create VPC stack
    fmt.Println("Step 1/3: Creating VPC stack...")
    vpcStart := time.Now()
    
    vpcResp, err := clustervpc.CreateVPC(ctx, &clustervpc.CreateVPCRequest{
        ClusterName:        req.ClusterName,
        VpcCidr:            req.VpcCidr,
        PublicSubnetCidrs:  req.PublicSubnetCidrs,
        PrivateSubnetCidrs: req.PrivateSubnetCidrs,
        AvailabilityZones:  req.AvailabilityZones,
        SingleNatGateway:   req.SingleNatGateway,
        AWSConfig:          req.AWSConfig,
    })
    if err != nil {
        return nil, fmt.Errorf("VPC stack creation failed: %w", err)
    }
    
    vpcEnd := time.Now()
    resp.VPC = &VPCResult{
        StackID:     vpcResp.StackID,
        Outputs:     vpcResp.Outputs,
        Duration:    vpcEnd.Sub(vpcStart),
        StartedAt:   vpcStart,
        CompletedAt: vpcEnd,
    }
    fmt.Printf("✓ VPC stack created (%s)\n\n", resp.VPC.Duration.Round(time.Second))
    
    // Step 2: Create OIDC provider stack
    fmt.Println("Step 2/3: Creating OIDC provider stack...")
    oidcStart := time.Now()
    
    oidcResp, err := createOIDCWithoutIAMUpdate(ctx, &clusteroidc.CreateOIDCRequest{
        ClusterName:    req.ClusterName,
        OIDCIssuerURL:  req.OIDCIssuerURL,
        OIDCThumbprint: req.OIDCThumbprint,
        AWSConfig:      req.AWSConfig,
    })
    if err != nil {
        return nil, fmt.Errorf("OIDC stack creation failed: %w", err)
    }
    
    oidcEnd := time.Now()
    resp.OIDC = &OIDCResult{
        StackID:     oidcResp.StackID,
        Outputs:     oidcResp.Outputs,
        Duration:    oidcEnd.Sub(oidcStart),
        StartedAt:   oidcStart,
        CompletedAt: oidcEnd,
    }
    fmt.Printf("✓ OIDC provider created (%s)\n\n", resp.OIDC.Duration.Round(time.Second))
    
    // Extract OIDC issuer domain for IAM stack
    oidcIssuerDomain, err := extractOIDCIssuerDomain(req.OIDCIssuerURL)
    if err != nil {
        return nil, fmt.Errorf("failed to extract OIDC issuer domain: %w", err)
    }
    
    // Step 3: Create IAM stack with correct trust policies
    fmt.Println("Step 3/3: Creating IAM stack with OIDC trust policies...")
    iamStart := time.Now()
    
    iamResp, err := clusteriam.CreateIAM(ctx, &clusteriam.CreateIAMRequest{
        ClusterName:      req.ClusterName,
        OIDCIssuerDomain: oidcIssuerDomain,
        AWSConfig:        req.AWSConfig,
    })
    if err != nil {
        return nil, fmt.Errorf("IAM stack creation failed: %w", err)
    }
    
    iamEnd := time.Now()
    resp.IAM = &IAMResult{
        StackID:     iamResp.StackID,
        Outputs:     iamResp.Outputs,
        Duration:    iamEnd.Sub(iamStart),
        StartedAt:   iamStart,
        CompletedAt: iamEnd,
    }
    fmt.Printf("✓ IAM stack created (%s)\n\n", resp.IAM.Duration.Round(time.Second))
    
    resp.TotalDuration = time.Since(totalStart)
    
    return resp, nil
}

// createOIDCWithoutIAMUpdate creates OIDC provider without updating IAM stack.
// This is a wrapper around clusteroidc.CreateOIDC that skips the IAM update step.
func createOIDCWithoutIAMUpdate(ctx context.Context, req *clusteroidc.CreateOIDCRequest) (*clusteroidc.CreateOIDCResponse, error) {
    // TODO: Need to refactor clusteroidc.CreateOIDC to make IAM update optional
    // For now, this is a placeholder showing the intent
    return clusteroidc.CreateOIDC(ctx, req)
}

func extractOIDCIssuerDomain(issuerURL string) (string, error) {
    // Reuse existing logic from clusteroidc package
    return crypto.GetOIDCIssuerDomain(issuerURL)
}
```

### 2. Refactor `internal/services/clusteroidc/service.go`

Make IAM trust policy update optional:

```go
type CreateOIDCRequest struct {
    ClusterName       string
    OIDCIssuerURL     string
    OIDCThumbprint    string
    AWSConfig         aws.Config
    SkipIAMUpdate     bool   // NEW: skip IAM trust policy update
}

func CreateOIDC(ctx context.Context, req *CreateOIDCRequest) (*CreateOIDCResponse, error) {
    // ... existing thumbprint and template logic ...
    
    // Create (or update) the OIDC provider stack
    output, err := cfnClient.CreateStack(ctx, cfParams)
    if err != nil {
        var alreadyExists *cloudformation.StackAlreadyExistsError
        if errors.As(err, &alreadyExists) {
            output, err = updateOIDCStack(ctx, cfnClient, req, oidcStackName, templateBody, thumbprint)
            if err != nil {
                return nil, err
            }
        } else {
            return nil, fmt.Errorf("failed to create OIDC stack: %w", err)
        }
    }
    
    // NEW: Only update IAM stack if not skipped
    if !req.SkipIAMUpdate {
        iamStackName := fmt.Sprintf("rosa-%s-iam", req.ClusterName)
        if err := updateIAMTrustPolicies(ctx, cfnClient, req.ClusterName, iamStackName, oidcIssuerDomain); err != nil {
            return nil, fmt.Errorf("OIDC provider created but failed to update IAM trust policies: %w", err)
        }
    }
    
    return &CreateOIDCResponse{
        StackID: output.StackID,
        Outputs: output.Outputs,
    }, nil
}
```

### 3. Add Timing to CloudFormation Client

Extend `internal/aws/cloudformation/stack.go`:

```go
// StackOutput contains the outputs from a CloudFormation stack
type StackOutput struct {
    StackID     string
    Outputs     map[string]string
    Duration    time.Duration  // NEW
    StartedAt   time.Time      // NEW
    CompletedAt time.Time      // NEW
}
```

Modify `internal/aws/cloudformation/client.go`:

```go
func (c *Client) CreateStack(ctx context.Context, params *CreateStackParams) (*StackOutput, error) {
    startTime := time.Now()  // NEW
    
    input := &cloudformation.CreateStackInput{
        StackName:    aws.String(params.StackName),
        TemplateBody: aws.String(params.TemplateBody),
        Capabilities: params.Capabilities,
        Tags:         params.Tags,
    }
    
    // ... existing parameter setup ...
    
    _, err := c.cfn.CreateStack(ctx, input)
    if err != nil {
        return nil, wrapError(err)
    }
    
    // Wait for stack creation to complete
    waiter := cloudformation.NewStackCreateCompleteWaiter(c.cfn)
    err = waiter.Wait(ctx, &cloudformation.DescribeStacksInput{
        StackName: aws.String(params.StackName),
    }, params.WaitTimeout)
    if err != nil {
        return nil, wrapError(err)
    }
    
    endTime := time.Now()  // NEW
    
    // Get stack outputs
    output, err := c.GetStackOutputs(ctx, params.StackName)
    if err != nil {
        return nil, err
    }
    
    // NEW: Add timing information
    output.Duration = endTime.Sub(startTime)
    output.StartedAt = startTime
    output.CompletedAt = endTime
    
    return output, nil
}
```

Similar changes for `UpdateStack` and `DeleteStack`.

### 4. Create `internal/commands/cluster/provision.go`

New CLI command:

```go
package cluster

import (
    "context"
    "fmt"
    "strings"
    "time"

    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/openshift-online/rosa-regional-platform-cli/internal/services/provision"
    "github.com/spf13/cobra"
)

type provisionOptions struct {
    clusterName        string
    region             string
    oidcIssuerURL      string
    oidcThumbprint     string
    vpcCidr            string
    publicSubnetCidrs  string
    privateSubnetCidrs string
    availabilityZones  string
    singleNatGateway   bool
}

func newProvisionCommand() *cobra.Command {
    opts := &provisionOptions{
        vpcCidr:            "10.0.0.0/16",
        publicSubnetCidrs:  "10.0.101.0/24,10.0.102.0/24,10.0.103.0/24",
        privateSubnetCidrs: "10.0.0.0/19,10.0.32.0/19,10.0.64.0/19",
        singleNatGateway:   true,
    }

    cmd := &cobra.Command{
        Use:   "provision CLUSTER_NAME",
        Short: "Provision cluster infrastructure (VPC, OIDC, IAM) in optimal order",
        Long: `Provision all AWS infrastructure for a hosted cluster in the optimal order
to minimize creation time.

This command creates stacks in this sequence:
1. VPC stack (networking resources) - 3-5 min
2. OIDC provider stack - 30s
3. IAM roles stack (with correct trust policies) - 1-2 min

Total time: ~5-8 minutes (vs. 16-24 min with traditional flow)

The traditional flow (cluster-vpc → cluster-iam → cluster-oidc) requires
an IAM trust policy UPDATE that triggers 10-15 min of eventual consistency
delay. This command eliminates that by creating IAM roles with the correct
OIDC issuer from the start.

Example:
  rosactl cluster provision my-cluster \
    --region us-east-1 \
    --oidc-issuer-url https://d1234.cloudfront.net/my-cluster

  # With custom VPC settings
  rosactl cluster provision my-cluster \
    --region us-east-1 \
    --oidc-issuer-url https://d1234.cloudfront.net/my-cluster \
    --vpc-cidr 10.1.0.0/16 \
    --availability-zones us-east-1a,us-east-1b,us-east-1c`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            opts.clusterName = args[0]
            return runProvision(cmd.Context(), opts)
        },
    }

    cmd.Flags().StringVar(&opts.region, "region", "", "AWS region (required)")
    cmd.Flags().StringVar(&opts.oidcIssuerURL, "oidc-issuer-url", "", "OIDC issuer URL from the cluster (required)")
    cmd.Flags().StringVar(&opts.oidcThumbprint, "oidc-thumbprint", "", "TLS thumbprint (optional, auto-fetched)")
    cmd.Flags().StringVar(&opts.vpcCidr, "vpc-cidr", opts.vpcCidr, "VPC CIDR block")
    cmd.Flags().StringVar(&opts.publicSubnetCidrs, "public-subnet-cidrs", opts.publicSubnetCidrs, "Comma-separated public subnet CIDRs")
    cmd.Flags().StringVar(&opts.privateSubnetCidrs, "private-subnet-cidrs", opts.privateSubnetCidrs, "Comma-separated private subnet CIDRs")
    cmd.Flags().StringVar(&opts.availabilityZones, "availability-zones", "", "Comma-separated AZs (optional, auto-detected)")
    cmd.Flags().BoolVar(&opts.singleNatGateway, "single-nat-gateway", opts.singleNatGateway, "Use single NAT gateway")

    _ = cmd.MarkFlagRequired("region")
    _ = cmd.MarkFlagRequired("oidc-issuer-url")

    return cmd
}

func runProvision(ctx context.Context, opts *provisionOptions) error {
    fmt.Println("🚀 Provisioning cluster infrastructure (deferred-IAM flow)")
    fmt.Printf("   Cluster: %s\n", opts.clusterName)
    fmt.Printf("   Region: %s\n", opts.region)
    fmt.Printf("   OIDC Issuer: %s\n", opts.oidcIssuerURL)
    fmt.Println()
    fmt.Println("This will create 3 stacks in optimal order:")
    fmt.Println("  1. VPC stack (networking)")
    fmt.Println("  2. OIDC provider stack")
    fmt.Println("  3. IAM roles stack (with correct trust policies)")
    fmt.Println()

    // Load AWS config
    cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(opts.region))
    if err != nil {
        return fmt.Errorf("failed to load AWS config: %w", err)
    }

    // Parse subnet CIDRs
    publicSubnets := strings.Split(opts.publicSubnetCidrs, ",")
    privateSubnets := strings.Split(opts.privateSubnetCidrs, ",")

    // Parse availability zones
    var azs []string
    if opts.availabilityZones != "" {
        azs = strings.Split(opts.availabilityZones, ",")
    }

    // Build provision request
    req := &provision.ProvisionRequest{
        ClusterName:        opts.clusterName,
        Region:             opts.region,
        VpcCidr:            opts.vpcCidr,
        PublicSubnetCidrs:  publicSubnets,
        PrivateSubnetCidrs: privateSubnets,
        AvailabilityZones:  azs,
        SingleNatGateway:   opts.singleNatGateway,
        OIDCIssuerURL:      opts.oidcIssuerURL,
        OIDCThumbprint:     opts.oidcThumbprint,
        AWSConfig:          cfg,
    }

    // Execute provisioning
    resp, err := provision.ProvisionInfrastructure(ctx, req)
    if err != nil {
        return err
    }

    // Print summary
    fmt.Println("═══════════════════════════════════════════════════════════")
    fmt.Println("✅ Infrastructure provisioned successfully!")
    fmt.Println("═══════════════════════════════════════════════════════════")
    fmt.Println()
    
    fmt.Println("Timing Summary:")
    fmt.Printf("  VPC stack:  %s\n", resp.VPC.Duration.Round(time.Second))
    fmt.Printf("  OIDC stack: %s\n", resp.OIDC.Duration.Round(time.Second))
    fmt.Printf("  IAM stack:  %s\n", resp.IAM.Duration.Round(time.Second))
    fmt.Println("  ─────────────────────────")
    fmt.Printf("  Total:      %s\n", resp.TotalDuration.Round(time.Second))
    fmt.Println()
    
    fmt.Println("Stack IDs:")
    fmt.Printf("  VPC:  %s\n", resp.VPC.StackID)
    fmt.Printf("  OIDC: %s\n", resp.OIDC.StackID)
    fmt.Printf("  IAM:  %s\n", resp.IAM.StackID)
    fmt.Println()
    
    fmt.Println("Key Outputs:")
    if vpcID, ok := resp.VPC.Outputs["VpcId"]; ok {
        fmt.Printf("  VPC ID: %s\n", vpcID)
    }
    if oidcArn, ok := resp.OIDC.Outputs["OIDCProviderArn"]; ok {
        fmt.Printf("  OIDC Provider ARN: %s\n", oidcArn)
    }
    if workerRole, ok := resp.IAM.Outputs["WorkerRoleArn"]; ok {
        fmt.Printf("  Worker Role ARN: %s\n", workerRole)
    }

    return nil
}
```

### 5. Wire up Command

Modify `internal/commands/cluster/cluster.go`:

```go
func NewCommand() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "cluster",
        Short: "Manage clusters",
    }

    cmd.AddCommand(newCreateCommand())
    cmd.AddCommand(newListCommand())
    cmd.AddCommand(newGetTokenCommand())
    cmd.AddCommand(newKubeconfigCommand())
    cmd.AddCommand(newAPICommand())
    cmd.AddCommand(newProvisionCommand())  // NEW

    return cmd
}
```

### 6. Add Query Command for Timing

Create `internal/commands/cluster/timings.go`:

```go
package cluster

import (
    "context"
    "fmt"
    "time"

    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/openshift-online/rosa-regional-platform-cli/internal/aws/cloudformation"
    "github.com/spf13/cobra"
)

type timingsOptions struct {
    clusterName string
    region      string
}

func newTimingsCommand() *cobra.Command {
    opts := &timingsOptions{}

    cmd := &cobra.Command{
        Use:   "timings CLUSTER_NAME",
        Short: "Show CloudFormation stack creation timings for a cluster",
        Long: `Display timing information for VPC, IAM, and OIDC stack creation.

This command queries CloudFormation stack metadata to show when each
stack was created and how long it took.

Example:
  rosactl cluster timings my-cluster --region us-east-1`,
        Args: cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            opts.clusterName = args[0]
            return runTimings(cmd.Context(), opts)
        },
    }

    cmd.Flags().StringVar(&opts.region, "region", "", "AWS region (required)")
    _ = cmd.MarkFlagRequired("region")

    return cmd
}

func runTimings(ctx context.Context, opts *timingsOptions) error {
    cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(opts.region))
    if err != nil {
        return fmt.Errorf("failed to load AWS config: %w", err)
    }

    cfnClient := cloudformation.NewClient(cfg)

    fmt.Printf("Stack Creation Timeline for: %s\n\n", opts.clusterName)

    stacks := []string{"vpc", "iam", "oidc"}
    
    for _, stackType := range stacks {
        stackName := fmt.Sprintf("rosa-%s-%s", opts.clusterName, stackType)
        
        stack, err := cfnClient.DescribeStack(ctx, stackName)
        if err != nil {
            var notFound *cloudformation.StackNotFoundError
            if errors.As(err, &notFound) {
                fmt.Printf("├─ %s (not found)\n", stackName)
                continue
            }
            return err
        }

        // Get stack events to find completion time
        events, err := cfnClient.GetStackEvents(ctx, stackName, 100)
        if err != nil {
            return err
        }

        var completedAt *time.Time
        for _, event := range events {
            if event.LogicalResourceID == stackName &&
               (event.ResourceStatus == "CREATE_COMPLETE" || 
                event.ResourceStatus == "UPDATE_COMPLETE") {
                completedAt = event.Timestamp
                break
            }
        }

        fmt.Printf("├─ %s\n", stackName)
        fmt.Printf("│  Created: %s\n", stack.CreationTime.Format(time.RFC3339))
        
        if completedAt != nil {
            duration := completedAt.Sub(*stack.CreationTime)
            fmt.Printf("│  Completed: %s\n", completedAt.Format(time.RFC3339))
            fmt.Printf("│  Duration: %s\n", duration.Round(time.Second))
        }
        
        fmt.Printf("│  Status: %s\n", stack.Status)
        fmt.Println("│")
    }

    return nil
}
```

Wire it up in `cluster.go`:

```go
cmd.AddCommand(newTimingsCommand())  // NEW
```

---

## Migration Path

### For New Clusters

Use the new `provision` command:

```bash
rosactl cluster provision my-cluster \
  --region us-east-1 \
  --oidc-issuer-url https://d1234.cloudfront.net/my-cluster
```

### For Existing Workflows

Continue using individual commands in the old order:

```bash
rosactl cluster-vpc create my-cluster --region us-east-1
rosactl cluster-iam create my-cluster --region us-east-1
rosactl cluster-oidc create my-cluster --oidc-issuer-url https://... --region us-east-1
```

### For Automation/CI

Update scripts to use the provision command for faster builds.

---

## Testing Strategy

### Unit Tests

1. Test `provision.ProvisionInfrastructure` with mocked CloudFormation client
2. Test timing instrumentation accuracy
3. Test OIDC issuer domain extraction

### Integration Tests (LocalStack)

1. Run full provision flow and verify stack creation order
2. Verify IAM roles have correct trust policies (no PENDING)
3. Compare timing between old flow vs. deferred-IAM flow
4. Test error handling at each step

### Example Test

```go
func TestProvisionInfrastructure_DeferredIAM(t *testing.T) {
    // Setup LocalStack
    ctx := context.Background()
    cfg := getLocalStackConfig(t)
    
    req := &provision.ProvisionRequest{
        ClusterName:   "test-cluster",
        Region:        "us-east-1",
        OIDCIssuerURL: "https://example.com/test-cluster",
        // ... other params
        AWSConfig: cfg,
    }
    
    resp, err := provision.ProvisionInfrastructure(ctx, req)
    require.NoError(t, err)
    
    // Verify stacks exist
    cfnClient := cloudformation.NewClient(cfg)
    vpcStack, err := cfnClient.DescribeStack(ctx, "rosa-test-cluster-vpc")
    require.NoError(t, err)
    
    oidcStack, err := cfnClient.DescribeStack(ctx, "rosa-test-cluster-oidc")
    require.NoError(t, err)
    
    iamStack, err := cfnClient.DescribeStack(ctx, "rosa-test-cluster-iam")
    require.NoError(t, err)
    
    // Verify IAM stack was created AFTER OIDC (check creation timestamps)
    assert.True(t, iamStack.CreationTime.After(*oidcStack.CreationTime))
    
    // Verify timing data
    assert.Greater(t, resp.VPC.Duration, time.Duration(0))
    assert.Greater(t, resp.OIDC.Duration, time.Duration(0))
    assert.Greater(t, resp.IAM.Duration, time.Duration(0))
    assert.Greater(t, resp.TotalDuration, time.Duration(0))
}
```

---

## Rollout Plan

### Week 1: Core Implementation
- [ ] Add `SkipIAMUpdate` flag to `clusteroidc.CreateOIDC`
- [ ] Add timing fields to `StackOutput` struct
- [ ] Implement timing capture in CloudFormation client

### Week 2: Provision Service
- [ ] Create `internal/services/provision/service.go`
- [ ] Write unit tests
- [ ] Update LocalStack integration tests

### Week 3: CLI Command
- [ ] Create `cluster provision` command
- [ ] Create `cluster timings` command
- [ ] Add command documentation

### Week 4: Testing & Documentation
- [ ] End-to-end testing with real AWS (vs. LocalStack)
- [ ] Performance benchmarking (old vs. new flow)
- [ ] Update `CLAUDE.md` and user guides
- [ ] Add examples to `examples/` directory

---

## Success Metrics

- **Timing**: Provision flow completes in 5-8 min (down from 16-24 min)
- **No IAM update**: IAM stack never updated after initial creation
- **Backward compatibility**: Existing commands still work
- **Observability**: Users can query historical timings with `cluster timings`

---

## Future Enhancements

1. **Parallel VPC + OIDC**: VPC and OIDC are independent — run in parallel to save ~5 min
2. **Async mode**: Return immediately, poll in background
3. **Progress streaming**: Real-time CloudFormation event streaming
4. **Retry logic**: Auto-retry transient CloudFormation failures
