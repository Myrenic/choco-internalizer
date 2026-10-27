package publish

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
)

type Client struct{ http *resty.Client }

type Package struct {
	ID           string `json:"id"`
	Version      string `json:"version"`
	Title        string `json:"title,omitempty"`
	Authors      string `json:"authors,omitempty"`
	Description  string `json:"description,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Tags         string `json:"tags,omitempty"`
	ProjectURL   string `json:"projectUrl,omitempty"`
	LicenseURL   string `json:"licenseUrl,omitempty"`
	Dependencies string `json:"dependencies,omitempty"`
	PackageSize  int64  `json:"packageSize"`
	PackageHash  string `json:"packageHash"`
}

func New() *Client {
	client := resty.New().SetTimeout(2 * time.Hour).SetRetryCount(3).
		SetRetryWaitTime(time.Second).SetRetryMaxWaitTime(15 * time.Second)
	client.AddRetryCondition(func(response *resty.Response, err error) bool {
		return err != nil || response.StatusCode() == http.StatusTooManyRequests || response.StatusCode() >= 500
	})
	return &Client{http: client}
}

// auth sets both credentials the NuGet surface can consume: the API key header
// that a NuGet push sends, and the bearer token the read routes need (the
// header is only honoured on the push route).
func (c *Client) auth(ctx context.Context, apiKey string) *resty.Request {
	return c.http.R().SetContext(ctx).
		SetHeader("X-NuGet-ApiKey", apiKey).
		SetHeader("Authorization", "Bearer "+apiKey)
}

func (c *Client) Push(ctx context.Context, repository, apiKey, packagePath string, pkg Package) (bool, error) {
	exists, err := c.Exists(ctx, repository, apiKey, pkg.ID, pkg.Version)
	if err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	root := strings.TrimRight(repository, "/")
	// NuGet v3 push surface: PUT <base>/api/v2/package with the package in a
	// multipart `package` field. The destination streams the body to a scratch
	// file, so a large package costs disk rather than memory and needs no
	// bespoke upload protocol.
	response, err := c.auth(ctx, apiKey).
		SetFile("package", packagePath).
		Put(root + "/api/v2/package")
	if err != nil {
		return false, fmt.Errorf("upload %s: %w", filepath.Base(packagePath), err)
	}
	switch response.StatusCode() {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
		return false, nil
	case http.StatusConflict:
		// The version landed between the existence check and the push.
		return true, nil
	}
	return false, fmt.Errorf("upload %s: repository returned HTTP %d", filepath.Base(packagePath), response.StatusCode())
}

func (c *Client) Exists(ctx context.Context, repository, apiKey, id, version string) (bool, error) {
	base, err := url.Parse(repository)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return false, fmt.Errorf("repository must be an absolute HTTPS URL")
	}
	root := strings.TrimRight(repository, "/")
	checkURL := fmt.Sprintf("%s/v2/package/%s/%s", root, url.PathEscape(id), url.PathEscape(version))
	response, err := c.auth(ctx, apiKey).Head(checkURL)
	if err != nil {
		return false, fmt.Errorf("check package existence: %w", err)
	}
	if response.StatusCode() == http.StatusOK {
		return true, nil
	}
	if response.StatusCode() == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("check package existence: repository returned HTTP %d", response.StatusCode())
}
