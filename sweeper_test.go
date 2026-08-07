package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/batonogov/terraform-provider-synology-dsm/internal/client"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// accTestPrefix is the naming convention every acceptance test follows. The
// sweepers below use it to decide what is safe to delete, so a test resource
// that does not carry this prefix will be left behind.
const accTestPrefix = "tfacctest"

// TestMain wires up the sweepers so that `go test ./... -sweep=all` removes
// leftovers before a run.
//
// Acceptance tests normally clean up after themselves, but a test that fails
// mid-step — or a run cancelled with Ctrl-C — leaves objects behind. The next
// run then fails on creation with error 3301 ("share already exists"), which
// looks like a code regression but is stale state. Sweeping first makes runs
// repeatable.
func TestMain(m *testing.M) {
	resource.TestMain(m)
}

func init() {
	resource.AddTestSweepers("dsm_shared_folder", &resource.Sweeper{
		Name: "dsm_shared_folder",
		F:    sweepSharedFolders,
	})
	resource.AddTestSweepers("dsm_user", &resource.Sweeper{
		Name: "dsm_user",
		F:    sweepUsers,
	})
	resource.AddTestSweepers("dsm_group", &resource.Sweeper{
		Name: "dsm_group",
		F:    sweepGroups,
	})
}

// sweeperClient builds a logged-in client from the same environment the
// acceptance tests use.
func sweeperClient() (*client.Client, error) {
	host := os.Getenv("SYNOLOGY_DSM_HOST")
	if host == "" {
		return nil, fmt.Errorf("SYNOLOGY_DSM_HOST must be set to run sweepers")
	}

	c := client.NewClient(
		host,
		os.Getenv("SYNOLOGY_DSM_USERNAME"),
		os.Getenv("SYNOLOGY_DSM_PASSWORD"),
		true,
	)
	if err := c.Login(context.Background()); err != nil {
		return nil, fmt.Errorf("sweeper login: %w", err)
	}
	return c, nil
}

func sweepSharedFolders(_ string) error {
	c, err := sweeperClient()
	if err != nil {
		return err
	}
	ctx := context.Background()

	shares, err := c.ListShares(ctx)
	if err != nil {
		return fmt.Errorf("sweep shared folders: %w", err)
	}

	var errs []string
	for _, share := range shares {
		if !strings.HasPrefix(share.Name, accTestPrefix) {
			continue
		}
		if err := c.DeleteShare(ctx, share.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", share.Name, err))
			continue
		}
		fmt.Printf("swept shared folder %s\n", share.Name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("sweep shared folders: %s", strings.Join(errs, "; "))
	}
	return nil
}

func sweepUsers(_ string) error {
	c, err := sweeperClient()
	if err != nil {
		return err
	}
	ctx := context.Background()

	users, err := c.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("sweep users: %w", err)
	}

	var errs []string
	for _, user := range users {
		if !strings.HasPrefix(user.Name, accTestPrefix) {
			continue
		}
		if err := c.DeleteUser(ctx, user.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", user.Name, err))
			continue
		}
		fmt.Printf("swept user %s\n", user.Name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("sweep users: %s", strings.Join(errs, "; "))
	}
	return nil
}

func sweepGroups(_ string) error {
	c, err := sweeperClient()
	if err != nil {
		return err
	}
	ctx := context.Background()

	groups, err := c.ListGroups(ctx)
	if err != nil {
		return fmt.Errorf("sweep groups: %w", err)
	}

	var errs []string
	for _, group := range groups {
		if !strings.HasPrefix(group.Name, accTestPrefix) {
			continue
		}
		if err := c.DeleteGroup(ctx, group.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", group.Name, err))
			continue
		}
		fmt.Printf("swept group %s\n", group.Name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("sweep groups: %s", strings.Join(errs, "; "))
	}
	return nil
}
