package aws

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
)

const (
	EnvRegion  = "ROSACTL_REGION"
	EnvProfile = "ROSACTL_PROFILE"
)

// ErrRegionRequired is returned when no region can be determined.
var ErrRegionRequired = errors.New("--region not set and could not be inferred from login URL: run 'rosactl login --url <URL>' or pass --region")

// Region returns the active AWS region.
func Region() string { return os.Getenv(EnvRegion) }

// Profile returns the active AWS profile.
func Profile() string { return os.Getenv(EnvProfile) }

// RequireRegion returns ErrRegionRequired if ROSACTL_REGION is not set.
func RequireRegion() error {
	if Region() == "" {
		return ErrRegionRequired
	}
	return nil
}

func NewConfig(ctx context.Context) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error

	if region := os.Getenv(EnvRegion); region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	if profile := os.Getenv(EnvProfile); profile != "" {
		// LoadSharedConfigProfile validates the profile exists; the SDK silently
		// falls back to the default profile if an unknown profile is passed to
		// LoadDefaultConfig, so this explicit check is necessary.
		//
		// Pass AWS_CONFIG_FILE / AWS_SHARED_CREDENTIALS_FILE explicitly so that
		// profiles defined only in non-default file locations are found. Without
		// this, LoadSharedConfigProfile always reads the default paths regardless
		// of those env vars (unlike LoadDefaultConfig which resolves them first).
		if _, err := config.LoadSharedConfigProfile(ctx, profile, func(o *config.LoadSharedConfigOptions) {
			if f := os.Getenv("AWS_CONFIG_FILE"); f != "" {
				o.ConfigFiles = []string{f}
			}
			if f := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); f != "" {
				o.CredentialsFiles = []string{f}
			}
		}); err != nil {
			return aws.Config{}, fmt.Errorf("profile %q not found: %w", profile, err)
		}
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}

	// FedRAMP SC-13 / IA-7: require FIPS 140-3 validated endpoints for all
	// AWS API calls. FIPS endpoints are required when operating in GovCloud
	// or any FedRAMP-authorized environment.
	opts = append(opts, config.WithUseFIPSEndpoint(aws.FIPSEndpointStateEnabled))

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}

	return cfg, nil
}
