// Package azuredevops provides a wrapper around Azure DevOps APIs
// in a manner compliant with the [forge.Forge] interface.
package azuredevops

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.abhg.dev/gs/internal/forge"
	"go.abhg.dev/gs/internal/silog"
	"go.abhg.dev/gs/internal/xec"
)

// DefaultURL is the default base URL for Azure DevOps Services.
const DefaultURL = "https://dev.azure.com"

// Options defines command line options for the Azure DevOps Forge.
// These are all hidden in the CLI,
// and are expected to be set only via environment variables.
type Options struct {
	// URL is the URL for Azure DevOps.
	// Override this for testing or Azure DevOps Server.
	URL string `name:"azuredevops-url" hidden:"" config:"forge.azuredevops.url" env:"AZURE_DEVOPS_URL" help:"Base URL for Azure DevOps web requests"`

	// Token is a fixed token used to authenticate with Azure DevOps.
	// This may be used to skip the login flow.
	Token string `name:"azuredevops-token" hidden:"" env:"AZURE_DEVOPS_PAT" help:"Azure DevOps Personal Access Token"`
}

// Forge builds an Azure DevOps Forge.
type Forge struct {
	Options Options

	// Log specifies the logger to use.
	Log *silog.Logger

	// Execer is the command executor.
	// If nil, the default executor is used.
	Execer xec.Execer
}

var _ forge.Forge = (*Forge)(nil)

func (f *Forge) logger() *silog.Logger {
	if f.Log == nil {
		return silog.Nop()
	}
	return f.Log.WithPrefix("azuredevops")
}

// URL returns the base URL configured for the Azure DevOps Forge
// or the default URL if none is set.
func (f *Forge) URL() string {
	return cmp.Or(f.Options.URL, DefaultURL)
}

// ID reports a unique key for this forge.
func (*Forge) ID() string { return "azuredevops" }

// CLIPlugin returns the CLI plugin for the Azure DevOps Forge.
func (f *Forge) CLIPlugin() any { return &f.Options }

// ParseRemoteURL parses an Azure DevOps remote URL
// and returns a [RepositoryID] if the URL matches.
//
// Supported formats:
//   - https://dev.azure.com/{org}/{project}/_git/{repo}
//   - git@ssh.dev.azure.com:v3/{org}/{project}/{repo}
func (f *Forge) ParseRemoteURL(remoteURL string) (forge.RepositoryID, error) {
	org, project, repo, err := extractRepoInfo(f.URL(), remoteURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", forge.ErrUnsupportedURL, err)
	}

	return &RepositoryID{
		url:          f.URL(),
		organization: org,
		project:      project,
		repository:   repo,
	}, nil
}

// OpenRepository opens the Azure DevOps repository
// that the given ID points to.
func (f *Forge) OpenRepository(
	ctx context.Context,
	tok forge.AuthenticationToken,
	id forge.RepositoryID,
) (forge.Repository, error) {
	rid := mustRepositoryID(id)
	adt := tok.(*AuthenticationToken)

	// For Azure CLI auth, refresh the token
	// by calling 'az account get-access-token'.
	// Azure CLI tokens expire after ~1 hour,
	// so we always fetch a fresh one.
	if adt.AuthType == AuthTypeAzureCLI {
		azExe, err := _execLookPath("az")
		if err != nil {
			return nil, fmt.Errorf(
				"refresh Azure CLI token: %w", err,
			)
		}

		freshToken, err := getAzureCLIToken(
			ctx, azExe, f.Execer,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"refresh Azure CLI token: %w", err,
			)
		}

		f.logger().Debug("Refreshed Azure CLI token")
		adt = &AuthenticationToken{
			AccessToken: freshToken,
			AuthType:    AuthTypeAzureCLI,
		}
	}

	// The Azure DevOps SDK expects an organization-scoped URL
	// (e.g. https://dev.azure.com/{org}),
	// not just the base URL.
	orgURL := f.URL() + "/" + rid.organization
	client, err := newAzureDevOpsClient(ctx, orgURL, adt)
	if err != nil {
		return nil, fmt.Errorf("create Azure DevOps client: %w", err)
	}

	return newRepository(ctx, f, rid, f.logger(), client)
}

// RepositoryID is a unique identifier for an Azure DevOps repository.
type RepositoryID struct {
	url          string // base URL (e.g., https://dev.azure.com)
	organization string // organization name
	project      string // project name
	repository   string // repository name
}

var _ forge.RepositoryID = (*RepositoryID)(nil)

func mustRepositoryID(id forge.RepositoryID) *RepositoryID {
	rid, ok := id.(*RepositoryID)
	if ok {
		return rid
	}
	panic(fmt.Sprintf("expected *RepositoryID, got %T", id))
}

// String returns a human-readable name for the repository ID.
func (rid *RepositoryID) String() string {
	return fmt.Sprintf("%s/%s/%s", rid.organization, rid.project, rid.repository)
}

// ChangeURL returns a URL to view a change on Azure DevOps.
func (rid *RepositoryID) ChangeURL(id forge.ChangeID) string {
	prID := mustPR(id).Number
	return fmt.Sprintf(
		"%s/%s/%s/_git/%s/pullrequest/%d",
		rid.url, rid.organization, rid.project, rid.repository, prID,
	)
}

func extractRepoInfo(
	baseURL, remoteURL string,
) (org, project, repo string, err error) {
	parsedBase, err := url.Parse(baseURL)
	if err != nil {
		return "", "", "", fmt.Errorf("bad base URL: %w", err)
	}

	// Handle SSH format: git@ssh.dev.azure.com:v3/{org}/{project}/{repo}
	if strings.HasPrefix(remoteURL, "git@ssh.dev.azure.com:") {
		return extractSSHRepoInfo(remoteURL)
	}

	// Normalize URL if it doesn't have a protocol.
	if !hasGitProtocol(remoteURL) && strings.Contains(remoteURL, ":") {
		remoteURL = "ssh://" + strings.Replace(remoteURL, ":", "/", 1)
	}

	u, err := url.Parse(remoteURL)
	if err != nil {
		return "", "", "", fmt.Errorf("parse remote URL: %w", err)
	}

	// Strip default ports if base URL doesn't specify a port.
	if parsedBase.Port() == "" {
		if host, port, err := net.SplitHostPort(u.Host); err == nil {
			switch port {
			case "443", "80":
				u.Host = host
			}
		}
	}

	// Check if the host matches the base URL.
	if u.Host != parsedBase.Host && !strings.HasSuffix(u.Host, "."+parsedBase.Host) {
		return "", "", "", fmt.Errorf(
			"%v is not an Azure DevOps URL: expected host %q, got %q",
			u, parsedBase.Host, u.Host,
		)
	}

	// Parse path: /{org}/{project}/_git/{repo}
	return extractHTTPSRepoInfo(u.Path)
}

func extractSSHRepoInfo(remoteURL string) (org, project, repo string, err error) {
	// Format: git@ssh.dev.azure.com:v3/{org}/{project}/{repo}
	s := strings.TrimPrefix(remoteURL, "git@ssh.dev.azure.com:")
	s = strings.TrimPrefix(s, "v3/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf(
			"invalid Azure DevOps SSH URL format: expected v3/{org}/{project}/{repo}, got %q",
			s,
		)
	}

	return parts[0], parts[1], parts[2], nil
}

func extractHTTPSRepoInfo(path string) (org, project, repo string, err error) {
	// Path format: /{org}/{project}/_git/{repo}
	s := strings.TrimPrefix(path, "/")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) < 4 {
		return "", "", "", fmt.Errorf(
			"invalid Azure DevOps URL path: expected /{org}/{project}/_git/{repo}, got %q",
			path,
		)
	}

	// Find the _git segment.
	gitIdx := -1
	for i, p := range parts {
		if p == "_git" {
			gitIdx = i
			break
		}
	}

	if gitIdx < 2 || gitIdx >= len(parts)-1 {
		return "", "", "", fmt.Errorf(
			"invalid Azure DevOps URL path: missing _git segment in %q",
			path,
		)
	}

	org = parts[0]
	project = parts[gitIdx-1]
	repo = parts[gitIdx+1]

	return org, project, repo, nil
}

// _gitProtocols is a list of known git protocols including the :// suffix.
var _gitProtocols = []string{
	"ssh://",
	"git://",
	"git+ssh://",
	"git+https://",
	"git+http://",
	"https://",
	"http://",
}

func hasGitProtocol(u string) bool {
	for _, proto := range _gitProtocols {
		if strings.HasPrefix(u, proto) {
			return true
		}
	}
	return false
}
