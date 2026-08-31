package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Registry mirrors one entry of Container Manager → Registry: a registry
// endpoint with credentials the Docker daemon uses to pull private images.
// The password is write-only on DSM's side too — method=get returns every
// field except it, which is what makes a Terraform resource with a
// password_wo argument possible without secrets ever landing in state.
type Registry struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Username       string `json:"username,omitempty"`
	EnableTrustSSC bool   `json:"enable_trust_SSC"`
	// Syno marks the built-in Docker Hub entry, which cannot be deleted.
	Syno bool `json:"syno"`
}

// ErrRegistryNotFound is returned by GetRegistryByURL when the registry list
// has no entry with the given URL.
var ErrRegistryNotFound = errors.New("registry not found")

type registryListResponse struct {
	Registries []Registry `json:"registries"`
}

// ListRegistries returns every registry Container Manager knows, including
// the built-in Docker Hub entry (Syno: true).
func (c *Client) ListRegistries(ctx context.Context) ([]Registry, error) {
	params := url.Values{}
	params.Set("limit", "-1")
	params.Set("offset", "0")
	data, err := c.DoAPIPost(ctx, "SYNO.Docker.Registry", "1", "get", params)
	if err != nil {
		return nil, fmt.Errorf("list registries: %w", err)
	}
	var resp registryListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("list registries: %w", err)
	}
	return resp.Registries, nil
}

// GetRegistryByURL finds a registry entry by its URL.
func (c *Client) GetRegistryByURL(ctx context.Context, registryURL string) (*Registry, error) {
	registries, err := c.ListRegistries(ctx)
	if err != nil {
		return nil, err
	}
	for i := range registries {
		if registries[i].URL == registryURL {
			return &registries[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrRegistryNotFound, registryURL)
}

// CreateRegistry registers a registry with credentials for pulling private
// images. The call mirrors what Container Manager's own UI sends, down to
// the JSON-quoted string values and the empty mirror list.
func (c *Client) CreateRegistry(ctx context.Context, name, registryURL, username, password string, trustSSC bool) error {
	if _, err := c.GetRegistryByURL(ctx, registryURL); err == nil {
		return fmt.Errorf("registry %q already exists; import it instead", registryURL)
	} else if !errors.Is(err, ErrRegistryNotFound) {
		return err
	}

	params := url.Values{}
	params.Set("name", jsonString(name))
	params.Set("url", jsonString(registryURL))
	params.Set("username", jsonString(username))
	params.Set("password", jsonString(password))
	params.Set("enable_registry_mirror", "false")
	params.Set("mirror_urls", "[]")
	params.Set("enable_trust_SSC", strconv.FormatBool(trustSSC))
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Registry", "1", "create", params); err != nil {
		return fmt.Errorf("create registry %q: %w", registryURL, err)
	}
	return nil
}

// DeleteRegistry removes a registry entry. DSM's delete call wants the
// entry name as well as the URL — a bare url is "invalid parameter" (code
// 101). The built-in Docker Hub entry (syno: true) is rejected by DSM itself.
func (c *Client) DeleteRegistry(ctx context.Context, name, registryURL string) error {
	params := url.Values{}
	params.Set("name", jsonString(name))
	params.Set("url", jsonString(registryURL))
	if _, err := c.DoAPIPost(ctx, "SYNO.Docker.Registry", "1", "delete", params); err != nil {
		return fmt.Errorf("delete registry %q: %w", registryURL, err)
	}
	return nil
}
