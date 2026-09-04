package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	v1alpha1 "github.com/openshift-online/rosa-hyperfleet-api/api/v1alpha1/public"
	hyperfleet "github.com/openshift-online/rosa-hyperfleet-api/clientset"
	"github.com/openshift-online/rosa-hyperfleet-api/clientset/platform"
	hfrest "github.com/openshift-online/rosa-hyperfleet-api/clientset/rest"
	"github.com/openshift-online/rosa-regional-platform-cli/internal/aws/cloudformation"
	pkgconfig "github.com/openshift-online/rosa-regional-platform-cli/internal/config"
)

// GenerateClusterConfigRequest contains parameters for generating cluster configuration
type GenerateClusterConfigRequest struct {
	ClusterName        string
	Region             string
	TargetProjectID    string
	Version            string
	ComputeReplicas    int
	ComputeMachineType string
	PlacementCluster   string
	Provider           string
	MultiAZ            bool
	LabelEnvironment   string
	LabelTeam          string
	// OidcConfigID, if set, selects the managed-OIDC flow: the platform API
	// looks up the referenced OidcConfig itself and derives
	// hostedCluster.issuerURL from it (that field is service-set and cannot
	// be supplied directly — see spec.oidcConfigId in ClusterSpec).
	OidcConfigID string
	AWSConfig    aws.Config
}

// GenerateClusterConfigResponse contains the generated cluster configuration
type GenerateClusterConfigResponse struct {
	Cluster *v1alpha1.Cluster
}

// SubmitClusterRequest contains parameters for submitting cluster to platform API
type SubmitClusterRequest struct {
	Cluster           *v1alpha1.Cluster
	PlatformAPIURL    string
	PlacementOverride string // Optional - overrides placement in payload if set
	AWSConfig         aws.Config
}

// SubmitClusterResponse contains the API response
type SubmitClusterResponse struct {
	Cluster *v1alpha1.Cluster
}

// GenerateClusterConfig generates a cluster configuration by querying CloudFormation stacks.
// If the IAM stack does not exist yet, role ARNs are computed from the cluster name and
// AWS account ID. This enables a provisioning flow where the IAM stack is created after
// the cluster (with the OIDC issuer URL known), avoiding an IAM trust policy UPDATE and
// the ~10-15 min eventual consistency delay it causes.
func GenerateClusterConfig(ctx context.Context, req *GenerateClusterConfigRequest) (*GenerateClusterConfigResponse, error) {
	cfnClient := cloudformation.NewClient(req.AWSConfig)

	// Get IAM outputs: prefer real stack, fall back to computed ARNs
	iamOutputs, err := getIAMOutputs(ctx, cfnClient, req.AWSConfig, req.ClusterName)
	if err != nil {
		return nil, err
	}

	// Get VPC stack information
	vpcStackName := fmt.Sprintf("rosa-%s-vpc", req.ClusterName)
	vpcStack, err := cfnClient.DescribeStack(ctx, vpcStackName)
	if err != nil {
		return nil, fmt.Errorf("failed to describe VPC stack: %w", err)
	}

	vpcID := vpcStack.Outputs["VpcId"]
	if vpcID == "" {
		return nil, fmt.Errorf("VPC stack %s missing required output VpcId", vpcStackName)
	}

	privateSubnetIDs := vpcStack.Outputs["PrivateSubnetIds"]
	if privateSubnetIDs == "" {
		return nil, fmt.Errorf("VPC stack %s missing required output PrivateSubnetIds", vpcStackName)
	}
	privateSubnets := strings.Split(privateSubnetIDs, ",")
	firstSubnet := strings.TrimSpace(privateSubnets[0])
	if firstSubnet == "" {
		return nil, fmt.Errorf("VPC stack %s has empty PrivateSubnetIds", vpcStackName)
	}

	workerInstanceProfile := iamOutputs["WorkerInstanceProfileName"]
	if workerInstanceProfile == "" {
		return nil, fmt.Errorf("IAM outputs missing required value WorkerInstanceProfileName")
	}

	spec := map[string]interface{}{
		"hostedCluster": map[string]interface{}{
			"release": map[string]interface{}{
				"image": req.Version,
			},
			"platform": map[string]interface{}{
				"type": "AWS",
				"aws": map[string]interface{}{
					"region": req.Region,
					"rolesRef": map[string]interface{}{
						"ingressARN":              iamOutputs["IngressRoleArn"],
						"imageRegistryARN":        iamOutputs["ImageRegistryRoleArn"],
						"storageARN":              iamOutputs["EBSCSIRoleArn"],
						"networkARN":              iamOutputs["NetworkConfigRoleArn"],
						"kubeCloudControllerARN":  iamOutputs["CloudControllerManagerRoleArn"],
						"nodePoolManagementARN":   iamOutputs["NodePoolManagementRoleArn"],
						"controlPlaneOperatorARN": iamOutputs["ControlPlaneOperatorRoleArn"],
					},
					"cloudProviderConfig": map[string]interface{}{
						"vpc":    vpcID,
						"zone":   req.Region + "a",
						"subnet": map[string]interface{}{"id": firstSubnet},
					},
				},
			},
		},
	}
	// oidcConfigId selects the managed-OIDC flow; the platform API resolves it
	// to hostedCluster.issuerURL itself (issuerURL cannot be set directly).
	spec["oidcConfigId"] = req.OidcConfigID

	// Build labels
	labels := map[string]interface{}{
		"environment": req.LabelEnvironment,
		"team":        req.LabelTeam,
		"region":      req.Region,
	}

	// Build the cluster object with proper Kubernetes structure
	clusterObj := map[string]interface{}{
		"kind": "Cluster",
		"metadata": map[string]interface{}{
			"name":   req.ClusterName,
			"labels": labels,
		},
		"spec": spec,
	}

	// Convert to typed struct
	clusterBytes, err := json.Marshal(clusterObj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal cluster: %w", err)
	}

	var cluster v1alpha1.Cluster
	if err := json.Unmarshal(clusterBytes, &cluster); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cluster: %w", err)
	}

	return &GenerateClusterConfigResponse{
		Cluster: &cluster,
	}, nil
}

// getIAMOutputs returns IAM role ARNs either from the existing CloudFormation stack
// or by computing them from the cluster name and AWS account ID.
func getIAMOutputs(ctx context.Context, cfnClient *cloudformation.Client, cfg aws.Config, clusterName string) (map[string]string, error) {
	iamStackName := fmt.Sprintf("rosa-%s-iam", clusterName)
	iamStack, err := cfnClient.DescribeStack(ctx, iamStackName)
	if err != nil {
		var notFound *cloudformation.StackNotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("failed to describe IAM stack: %w", err)
		}

		accountID, stsErr := getAWSAccountID(ctx, cfg)
		if stsErr != nil {
			return nil, fmt.Errorf("IAM stack not found and failed to get AWS account ID: %w", stsErr)
		}

		fmt.Printf("IAM stack %s not found — computing role ARNs from account %s\n", iamStackName, accountID)
		return computeIAMRoleARNs(clusterName, accountID), nil
	}

	return iamStack.Outputs, nil
}

func getAWSAccountID(ctx context.Context, cfg aws.Config) (string, error) {
	stsClient := sts.NewFromConfig(cfg)
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(identity.Account), nil
}

// computeIAMRoleARNs returns the same output map that the rosa-{cluster}-iam
// CloudFormation stack would produce. Role names match the template exactly.
func computeIAMRoleARNs(clusterName, accountID string) map[string]string {
	arn := func(roleName string) string {
		return fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, roleName)
	}

	return map[string]string{
		"IngressRoleArn":                arn(clusterName + "-ingress"),
		"CloudControllerManagerRoleArn": arn(clusterName + "-cloud-controller-manager"),
		"EBSCSIRoleArn":                 arn(clusterName + "-ebs-csi"),
		"ImageRegistryRoleArn":          arn(clusterName + "-image-registry"),
		"NetworkConfigRoleArn":          arn(clusterName + "-network-config"),
		"ControlPlaneOperatorRoleArn":   arn(clusterName + "-control-plane-operator"),
		"NodePoolManagementRoleArn":     arn(clusterName + "-node-pool-management"),
		"WorkerRoleArn":                 arn(clusterName + "-ROSA-Worker-Role"),
		"WorkerInstanceProfileName":     clusterName + "-ROSA-Worker-Role",
	}
}

// SubmitCluster submits a cluster configuration to the platform API
func SubmitCluster(ctx context.Context, req *SubmitClusterRequest) (*SubmitClusterResponse, error) {
	cluster := req.Cluster

	// Override placement if specified
	if req.PlacementOverride != "" {
		// Need to convert to map, modify, then convert back to preserve placement override
		clusterBytes, err := json.Marshal(cluster)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal cluster: %w", err)
		}

		var clusterMap map[string]interface{}
		if err := json.Unmarshal(clusterBytes, &clusterMap); err != nil {
			return nil, fmt.Errorf("failed to unmarshal to map: %w", err)
		}

		if spec, ok := clusterMap["spec"].(map[string]interface{}); ok {
			spec["placement"] = req.PlacementOverride
		}

		// Convert back to typed struct
		modifiedBytes, err := json.Marshal(clusterMap)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal modified cluster: %w", err)
		}

		var modifiedCluster v1alpha1.Cluster
		if err := json.Unmarshal(modifiedBytes, &modifiedCluster); err != nil {
			return nil, fmt.Errorf("failed to unmarshal modified cluster: %w", err)
		}
		cluster = &modifiedCluster
	}

	cs, err := newHyperfleetClientset(ctx, req.PlatformAPIURL)
	if err != nil {
		return nil, err
	}

	// Create cluster via clientset
	createdCluster, err := cs.HyperfleetV1alpha1().Clusters().Create(ctx, cluster, platform.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create cluster: %w", err)
	}

	return &SubmitClusterResponse{
		Cluster: createdCluster,
	}, nil
}

// newHyperfleetClientset builds a platform API clientset authenticated via the
// caller's default AWS credentials (SigV4).
func newHyperfleetClientset(ctx context.Context, platformAPIURL string) (*hyperfleet.Clientset, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	accountID, err := pkgconfig.GetAccountID()
	if err != nil {
		return nil, fmt.Errorf("failed to get account ID: %w", err)
	}

	cs, err := hyperfleet.NewForConfig(&hfrest.Config{
		Host:      platformAPIURL,
		AccountID: accountID,
		AWSConfig: awsCfg,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return cs, nil
}

// toCamelCase converts a PascalCase string to camelCase
// Examples: "VpcId" -> "vpcId", "OIDCProviderArn" -> "oidcProviderArn"
func toCamelCase(s string) string {
	if s == "" {
		return s
	}

	runes := []rune(s)

	// Find the end of the leading uppercase sequence
	// For "OIDCProviderArn", we want to lowercase "OIDC" but keep "P" uppercase
	uppercaseEnd := 0
	for i := 0; i < len(runes); i++ {
		if !unicode.IsUpper(runes[i]) {
			// Found a non-uppercase character
			break
		}
		uppercaseEnd = i
	}

	// If we have multiple uppercase letters followed by a lowercase letter,
	// we should keep the last uppercase as-is (it starts the next word)
	// e.g., "OIDCProvider" -> uppercase until 'P', keep 'P' uppercase
	if uppercaseEnd > 0 && uppercaseEnd+1 < len(runes) && unicode.IsLower(runes[uppercaseEnd+1]) {
		uppercaseEnd--
	}

	// Convert the leading uppercase sequence to lowercase
	var result strings.Builder
	for i := 0; i <= uppercaseEnd; i++ {
		result.WriteRune(unicode.ToLower(runes[i]))
	}

	// Append the rest unchanged
	for i := uppercaseEnd + 1; i < len(runes); i++ {
		result.WriteRune(runes[i])
	}

	return result.String()
}
