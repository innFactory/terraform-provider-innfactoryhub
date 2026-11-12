package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	install "github.com/hashicorp/hc-install"
	"github.com/hashicorp/hc-install/checkpoint"
	"github.com/hashicorp/hc-install/fs"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hc-install/src"
)

// Variables of terraform-plugin-testing that choose the Terraform CLI.
const (
	envTerraformPath    = "TF_ACC_TERRAFORM_PATH"
	envTerraformVersion = "TF_ACC_TERRAFORM_VERSION"
	envTempDir          = "TF_ACC_TEMP_DIR"
)

// terraformInstallTimeout bounds the one download of the Terraform CLI
// (index, checksums, ~30 MB archive, unpacking under -race).
const terraformInstallTimeout = 3 * time.Minute

// TestMain resolves the Terraform CLI once for the whole package and hands it
// to every resource.UnitTest via TF_ACC_TERRAFORM_PATH.
//
// Without it, terraform-plugin-testing resolves the CLI per test: where no
// terraform is on PATH (GitHub runners) it downloads and unpacks a full
// release into a fresh directory for each of the ~70 resource tests, about
// 10 s each under -race, which pushed `go test` past its 10-minute limit.
func TestMain(m *testing.M) {
	os.Exit(runWithTerraformCLI(m))
}

func runWithTerraformCLI(m *testing.M) int {
	if os.Getenv(envTerraformPath) != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp(os.Getenv(envTempDir), "terraform-cli")
	if err != nil {
		fmt.Fprintf(os.Stderr, "terraform cli: create install dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()

	execPath, err := ensureTerraformCLI(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "terraform cli: %v (put terraform on PATH or set %s)\n", err, envTerraformPath)
		return 1
	}
	if err := os.Setenv(envTerraformPath, execPath); err != nil {
		fmt.Fprintf(os.Stderr, "terraform cli: set %s: %v\n", envTerraformPath, err)
		return 1
	}
	return m.Run()
}

// ensureTerraformCLI finds or installs the CLI from the same sources, in the
// same order, as terraform-plugin-testing: the exact TF_ACC_TERRAFORM_VERSION
// if set, otherwise terraform on PATH, otherwise the latest release. Installs
// go to dir.
func ensureTerraformCLI(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), terraformInstallTimeout)
	defer cancel()

	var sources []src.Source
	if raw := strings.TrimPrefix(os.Getenv(envTerraformVersion), "v"); raw != "" {
		v, err := version.NewVersion(raw)
		if err != nil {
			return "", fmt.Errorf("invalid %s %q: %w", envTerraformVersion, raw, err)
		}
		sources = append(sources, &releases.ExactVersion{
			Product:    product.Terraform,
			Version:    v,
			InstallDir: dir,
			Timeout:    terraformInstallTimeout,
		})
	} else {
		sources = append(sources,
			&fs.AnyVersion{Product: &product.Terraform},
			&checkpoint.LatestVersion{
				Product:    product.Terraform,
				InstallDir: dir,
				Timeout:    terraformInstallTimeout,
			},
		)
	}

	execPath, err := install.NewInstaller().Ensure(ctx, sources)
	if err != nil {
		return "", fmt.Errorf("find or install: %w", err)
	}
	return execPath, nil
}
