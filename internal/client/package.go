package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrPackageNotFound = errors.New("DSM package not found")

// Package Center operations are asynchronous on physical NAS devices. These
// are variables so unit tests can use millisecond-scale polling.
var (
	packagePollInterval = 2 * time.Second
	packageTaskTimeout  = 15 * time.Minute
)

// Package is an installed DSM package. DSM returns most of these fields inside
// an `additional` object even though older versions returned some at top level.
type Package struct {
	ID           string
	Name         string
	Version      string
	Status       string
	Description  string
	Maintainer   string
	CanUninstall bool
}

func (p Package) Running() bool {
	return strings.EqualFold(p.Status, "running")
}

// PackageCatalogItem is the subset of Package Center catalog metadata that
// must be echoed back into the install queue and install calls.
type PackageCatalogItem struct {
	ID                   string
	Version              string
	Source               string
	DownloadURL          string
	Checksum             string
	InstallType          string
	Size                 int64
	Type                 int
	Beta                 bool
	InstallOnColdStorage bool
	Dependencies         json.RawMessage
	Dependents           json.RawMessage
	Conflicts            json.RawMessage
	Breaks               json.RawMessage
	Replaces             json.RawMessage
}

// ListPackages returns every installed package and asks DSM to include the
// fields required to observe its lifecycle state.
func (c *Client) ListPackages(ctx context.Context) ([]Package, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "-1")
	params.Set("additional", `["status","ctl_uninstall","description","maintainer"]`)

	data, err := c.DoAPI(ctx, "SYNO.Core.Package", "2", "list", params)
	if err != nil {
		// DSM 6 and early DSM 7 builds only accept v1. It may omit some
		// additional fields, but still supplies enough data for lookup/import.
		fallbackParams := url.Values{}
		fallbackParams.Set("offset", "0")
		fallbackParams.Set("limit", "-1")
		data, err = c.DoAPI(ctx, "SYNO.Core.Package", "1", "list", fallbackParams)
		if err != nil {
			return nil, fmt.Errorf("list packages: %w", err)
		}
	}

	var result struct {
		Packages []json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse package list: %w", err)
	}

	packages := make([]Package, 0, len(result.Packages))
	for _, raw := range result.Packages {
		pkg, err := parsePackage(raw)
		if err != nil || pkg.ID == "" {
			continue
		}
		packages = append(packages, *pkg)
	}
	return packages, nil
}

func (c *Client) GetPackage(ctx context.Context, id string) (*Package, error) {
	packages, err := c.ListPackages(ctx)
	if err != nil {
		return nil, err
	}
	for i := range packages {
		if packages[i].ID == id {
			return &packages[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrPackageNotFound, id)
}

// InstallPackage installs an official or configured Package Center package,
// including dependencies returned by DSM's global install queue.
func (c *Client) InstallPackage(ctx context.Context, id, volume string, run bool) (*Package, error) {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()

	if pkg, err := c.GetPackage(ctx, id); err == nil {
		return pkg, nil
	} else if !errors.Is(err, ErrPackageNotFound) {
		return nil, err
	}

	if volume == "" {
		volume = "/volume1"
	}

	catalog, err := c.listPackageCatalog(ctx)
	if err != nil {
		return nil, err
	}
	requested, ok := findCatalogPackage(catalog, id)
	if !ok {
		return nil, fmt.Errorf("%w in Package Center catalog: %q", ErrPackageNotFound, id)
	}
	if checkedVolume, err := c.checkPackageInstall(ctx, requested, volume); err != nil {
		return nil, err
	} else if checkedVolume != "" {
		volume = checkedVolume
	}

	queue, err := c.getPackageInstallQueue(ctx, requested)
	if err != nil {
		return nil, err
	}
	if len(queue) == 0 {
		queue = []packageQueueItem{{ID: requested.ID}}
	}

	for _, queued := range queue {
		if queued.ID == "" {
			return nil, errors.New("DSM returned an install queue item without a package identifier")
		}
		meta, found := findCatalogPackage(catalog, queued.ID)
		if !found {
			meta = PackageCatalogItem{ID: queued.ID}
		}
		installVolume := volume
		if queued.Volume != "" {
			installVolume = queued.Volume
		}
		// Dependencies follow their Package Center defaults. Only the package
		// named by the Terraform resource follows the requested running state.
		startAfterInstall := queued.ID != id || run
		if err := c.startPackageInstall(ctx, meta, installVolume, startAfterInstall); err != nil {
			return nil, fmt.Errorf("install package %q: %w", queued.ID, err)
		}
	}

	return c.waitForPackage(ctx, id, func(pkg *Package) bool {
		return !isTransientPackageStatus(pkg.Status)
	})
}

// SetPackageRunning starts or stops a package and waits until DSM reports the
// requested stable state.
func (c *Client) SetPackageRunning(ctx context.Context, id string, running bool) (*Package, error) {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()

	method := "stop"
	if running {
		method = "start"
	}

	params := url.Values{}
	params.Set("id", id)
	if _, err := c.DoAPIPost(ctx, "SYNO.Core.Package.Control", "1", method, params); err != nil {
		return nil, fmt.Errorf("%s package %q: %w", method, id, err)
	}

	return c.waitForPackage(ctx, id, func(pkg *Package) bool {
		return pkg.Running() == running && !isTransientPackageStatus(pkg.Status)
	})
}

// UninstallPackage permanently removes a package. Callers are expected to
// expose this only behind an explicit destructive opt-in.
func (c *Client) UninstallPackage(ctx context.Context, id string) error {
	c.packageMu.Lock()
	defer c.packageMu.Unlock()

	pkg, err := c.GetPackage(ctx, id)
	if errors.Is(err, ErrPackageNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !pkg.CanUninstall {
		return fmt.Errorf("package %q cannot be uninstalled according to DSM", id)
	}

	params := url.Values{}
	params.Set("id", id)
	params.Set("dsm_apps", "")
	if _, err := c.DoAPIPost(ctx, "SYNO.Core.Package.Uninstallation", "1", "uninstall", params); err != nil {
		return fmt.Errorf("uninstall package %q: %w", id, err)
	}

	deadline := time.NewTimer(packageTaskTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(packagePollInterval)
	defer ticker.Stop()

	for {
		_, err := c.GetPackage(ctx, id)
		if errors.Is(err, ErrPackageNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wait for package %q uninstall: %w", id, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("package %q was not uninstalled within %s", id, packageTaskTimeout)
		case <-ticker.C:
		}
	}
}

func (c *Client) listPackageCatalog(ctx context.Context) ([]PackageCatalogItem, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "-1")
	params.Set("blqinst", "true")
	params.Set("lang", "enu")
	params.Set("additional", `["beta","version","size","link","md5","source","type","deppkgs","depsers","conflictpkgs","breakpkgs","replacepkgs","install_type","install_on_cold_storage"]`)

	data, v2Err := c.DoAPI(ctx, "SYNO.Core.Package.Server", "2", "list", params)
	if v2Err == nil {
		items, err := parsePackageCatalog(data)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			return items, nil
		}
		// Some DSM 7 builds accept v2 but silently return an empty catalog.
		// Retry with v1, which is also what older Package Center uses.
	}

	data, err := c.DoAPI(ctx, "SYNO.Core.Package.Server", "1", "list", params)
	if err != nil {
		if v2Err != nil {
			return nil, fmt.Errorf("list Package Center catalog: v2: %v; v1: %w", v2Err, err)
		}
		return nil, fmt.Errorf("list Package Center catalog: %w", err)
	}
	return parsePackageCatalog(data)
}

func parsePackageCatalog(data json.RawMessage) ([]PackageCatalogItem, error) {
	var result struct {
		Packages []json.RawMessage `json:"packages"`
		Data     []json.RawMessage `json:"data"`
		List     []json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse Package Center catalog: %w", err)
	}

	rawItems := result.Packages
	if len(rawItems) == 0 {
		rawItems = result.Data
	}
	if len(rawItems) == 0 {
		rawItems = result.List
	}

	items := make([]PackageCatalogItem, 0, len(rawItems))
	for _, raw := range rawItems {
		item, err := parseCatalogPackage(raw)
		if err != nil || item.ID == "" {
			continue
		}
		items = append(items, *item)
	}
	return items, nil
}

type packageQueueItem struct {
	ID     string `json:"pkg"`
	Volume string `json:"volume,omitempty"`
}

func (c *Client) checkPackageInstall(ctx context.Context, pkg PackageCatalogItem, requestedVolume string) (string, error) {
	params := url.Values{}
	params.Set("id", pkg.ID)
	params.Set("blupgrade", "false")
	params.Set("blCheckDep", "false")
	if pkg.Version != "" {
		params.Set("ver", pkg.Version)
	}
	if pkg.Size > 0 {
		params.Set("size", strconv.FormatInt(pkg.Size, 10))
	}
	if pkg.InstallType != "" {
		params.Set("install_type", pkg.InstallType)
	}
	if pkg.InstallOnColdStorage {
		params.Set("install_on_cold_storage", "true")
	}
	setPackageRawParam(params, "deppkgs", pkg.Dependencies)
	setPackageRawParam(params, "depsers", pkg.Dependents)
	setPackageRawParam(params, "conflictpkgs", pkg.Conflicts)
	setPackageRawParam(params, "breakpkgs", pkg.Breaks)
	setPackageRawParam(params, "replacepkgs", pkg.Replaces)

	data, err := c.DoAPIPost(ctx, "SYNO.Core.Package.Installation", "2", "check", params)
	if err != nil {
		// Several DSM 7 builds reject the advisory check unless Package Center
		// has cached extra UI metadata. The queue/install calls remain the real
		// gate, so tolerate only the known method/parameter/catalog errors.
		if isPackageAPIError(err, 103, 104, 114, 120) {
			return requestedVolume, nil
		}
		return "", fmt.Errorf("check package %q installation: %w", pkg.ID, err)
	}

	var result struct {
		IsOccupied json.RawMessage `json:"is_occupied"`
		VolumePath string          `json:"volume_path"`
		Volumes    []struct {
			MountPoint string `json:"mount_point"`
		} `json:"volume_list"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse package %q installation check: %w", pkg.ID, err)
	}
	if rawBool(result.IsOccupied) {
		return "", errors.New("DSM Package Center is busy")
	}
	if requestedVolume != "" {
		return requestedVolume, nil
	}
	if result.VolumePath != "" {
		return result.VolumePath, nil
	}
	for _, volume := range result.Volumes {
		if volume.MountPoint != "" {
			return volume.MountPoint, nil
		}
	}
	return "/volume1", nil
}

func (c *Client) getPackageInstallQueue(ctx context.Context, pkg PackageCatalogItem) ([]packageQueueItem, error) {
	request, err := json.Marshal([]map[string]interface{}{{
		"pkg":       pkg.ID,
		"operation": "install",
		"version":   pkg.Version,
		"beta":      pkg.Beta,
	}})
	if err != nil {
		return nil, fmt.Errorf("encode package install queue: %w", err)
	}

	params := url.Values{}
	params.Set("pkgs", string(request))
	data, err := c.DoAPIPost(ctx, "SYNO.Core.Package.Installation", "1", "get_queue", params)
	if err != nil {
		return nil, fmt.Errorf("resolve package install queue: %w", err)
	}

	var result struct {
		Queue          []packageQueueItem `json:"queue"`
		Missing        []json.RawMessage  `json:"non_exist_pkgs"`
		Conflicts      []json.RawMessage  `json:"conflicted_pkgs"`
		Broken         []json.RawMessage  `json:"broken_pkgs"`
		Paused         []json.RawMessage  `json:"paused_pkgs"`
		CausingPausing []json.RawMessage  `json:"cause_pausing_pkgs"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse package install queue: %w", err)
	}
	if details := firstPackageQueueProblem(result.Missing, result.Conflicts, result.Broken, result.Paused, result.CausingPausing); details != "" {
		return nil, fmt.Errorf("package install queue rejected %q: %s", pkg.ID, details)
	}
	return result.Queue, nil
}

func (c *Client) startPackageInstall(ctx context.Context, pkg PackageCatalogItem, volume string, run bool) error {
	params := url.Values{}
	params.Set("name", pkg.ID)
	params.Set("blqinst", "true")
	params.Set("volume_path", volume)
	params.Set("is_syno", strconv.FormatBool(pkg.isSynologyPackage()))
	params.Set("beta", strconv.FormatBool(pkg.Beta))
	params.Set("installrunpackage", strconv.FormatBool(run))

	if !pkg.isSynologyPackage() && pkg.DownloadURL != "" {
		params.Set("url", pkg.DownloadURL)
		params.Set("operation", "install")
		params.Set("checksum", pkg.Checksum)
		if pkg.Size > 0 {
			params.Set("filesize", strconv.FormatInt(pkg.Size, 10))
		}
		if pkg.Type != 0 {
			params.Set("type", strconv.Itoa(pkg.Type))
		}
	}

	data, err := c.DoAPIPost(ctx, "SYNO.Core.Package.Installation", "1", "install", params)
	if err != nil {
		return err
	}
	var result struct {
		Error    json.RawMessage `json:"error"`
		Code     int             `json:"code"`
		Progress float64         `json:"progress"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("parse package install response: %w", err)
		}
	}
	if result.Code != 0 {
		return fmt.Errorf("DSM returned package install code %d", result.Code)
	}
	if message := rawString(result.Error); message != "" {
		return errors.New(message)
	}
	if result.Progress < 0 {
		return errors.New("DSM rejected the package install request")
	}
	return nil
}

func (c *Client) waitForPackage(ctx context.Context, id string, ready func(*Package) bool) (*Package, error) {
	deadline := time.NewTimer(packageTaskTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(packagePollInterval)
	defer ticker.Stop()

	for {
		pkg, err := c.GetPackage(ctx, id)
		if err == nil && strings.EqualFold(pkg.Status, "broken") {
			return nil, fmt.Errorf("package %q is broken", id)
		}
		if err == nil && ready(pkg) {
			return pkg, nil
		}
		if err != nil && !errors.Is(err, ErrPackageNotFound) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("package %q did not reach the requested state within %s", id, packageTaskTimeout)
		case <-ticker.C:
		}
	}
}

func parsePackage(raw json.RawMessage) (*Package, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}

	pkg := &Package{
		ID:          rawString(object["id"]),
		Name:        rawString(object["name"]),
		Version:     rawString(object["version"]),
		Status:      rawString(object["status"]),
		Description: rawString(object["description"]),
		Maintainer:  rawString(object["maintainer"]),
	}
	pkg.CanUninstall = rawBool(object["ctl_uninstall"])

	var additional map[string]json.RawMessage
	if json.Unmarshal(object["additional"], &additional) == nil {
		if pkg.Status == "" {
			pkg.Status = rawString(additional["status"])
		}
		if pkg.Description == "" {
			pkg.Description = rawString(additional["description"])
		}
		if pkg.Maintainer == "" {
			pkg.Maintainer = rawString(additional["maintainer"])
		}
		if !pkg.CanUninstall {
			pkg.CanUninstall = rawBool(additional["ctl_uninstall"])
		}
	}
	return pkg, nil
}

func parseCatalogPackage(raw json.RawMessage) (*PackageCatalogItem, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}

	id := rawString(object["id"])
	if id == "" {
		id = rawString(object["package"])
	}
	return &PackageCatalogItem{
		ID:                   id,
		Version:              rawString(object["version"]),
		Source:               rawString(object["source"]),
		DownloadURL:          rawString(object["link"]),
		Checksum:             rawString(object["md5"]),
		InstallType:          rawString(object["install_type"]),
		Size:                 rawInt64(object["size"]),
		Type:                 int(rawInt64(object["type"])),
		Beta:                 rawBool(object["beta"]),
		InstallOnColdStorage: rawBool(object["install_on_cold_storage"]),
		Dependencies:         object["deppkgs"],
		Dependents:           object["depsers"],
		Conflicts:            object["conflictpkgs"],
		Breaks:               object["breakpkgs"],
		Replaces:             object["replacepkgs"],
	}, nil
}

func (p PackageCatalogItem) isSynologyPackage() bool {
	switch strings.ToLower(p.Source) {
	case "", "syno", "synology":
		return true
	default:
		return false
	}
}

func findCatalogPackage(catalog []PackageCatalogItem, id string) (PackageCatalogItem, bool) {
	for _, item := range catalog {
		if item.ID == id {
			return item, true
		}
	}
	return PackageCatalogItem{}, false
}

func firstPackageQueueProblem(groups ...[]json.RawMessage) string {
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		parts := make([]string, 0, len(group))
		for _, item := range group {
			value := strings.TrimSpace(string(item))
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			parts = append(parts, value)
		}
		return strings.Join(parts, ", ")
	}
	return ""
}

func isTransientPackageStatus(status string) bool {
	switch strings.ToLower(status) {
	case "installing", "downloading", "queueing", "starting", "stopping", "upgrading", "repairing", "loading", "uninstalling":
		return true
	default:
		return false
	}
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func rawBool(raw json.RawMessage) bool {
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var number int
	if json.Unmarshal(raw, &number) == nil {
		return number != 0
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, _ = strconv.ParseBool(text)
		return value
	}
	return false
}

func rawInt64(raw json.RawMessage) int64 {
	var value int64
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, _ = strconv.ParseInt(text, 10, 64)
	}
	return value
}

func setPackageRawParam(params url.Values, name string, raw json.RawMessage) {
	value := strings.TrimSpace(string(raw))
	if value != "" && value != "null" {
		params.Set(name, value)
	}
}

func isPackageAPIError(err error, codes ...int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, code := range codes {
		if apiErr.Code == code {
			return true
		}
	}
	return false
}
