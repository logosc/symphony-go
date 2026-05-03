package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v68/github"
)

// AppAuth holds the credentials of a GitHub App installation, used by
// NewAppClient to authenticate API calls.
//
// Compared to a personal access token (NewClient), App auth gives:
//   - scoping to specific repositories at install time
//   - automatic hourly token rotation handled by ghinstallation
//   - revocation by uninstalling the App in one place on github.com
//
// PrivateKeyPEM is the contents of the .pem file downloaded from the
// GitHub App's settings page (a PKCS#1 RSA private key inside an
// "RSA PRIVATE KEY" PEM block).
type AppAuth struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

// AppCreds is anything that can mint a fresh, short-lived GitHub App
// installation access token on demand. *ghinstallation.Transport satisfies
// it. Used by callers that need a token outside the API client itself —
// notably `git push` over HTTPS with the token in an `extraheader`.
type AppCreds interface {
	Token(ctx context.Context) (string, error)
}

// NewAppClient constructs a Client authenticated as a GitHub App
// installation, plus an AppCreds source the caller can use to mint a
// fresh installation access token (e.g. for `git push`). The returned
// Client is interchangeable with one returned by NewClient — it satisfies
// the same interface and the orchestrator does not need to know which
// auth scheme is in use.
//
// JWT signing and installation-access-token refresh are handled
// transparently by ghinstallation. The orchestrator never persists the
// short-lived installation token; for any operation that actually needs
// the bearer (push), it calls AppCreds.Token(ctx) at the point of use.
//
// fullName must be of the form "OWNER/REPO". The App installation must
// have access to that repository — this is not validated at construction
// time; the first API call will surface the failure.
func NewAppClient(ctx context.Context, auth AppAuth, fullName string) (Client, AppCreds, error) {
	if auth.AppID == 0 {
		return nil, nil, fmt.Errorf("github: AppAuth.AppID must be non-zero")
	}
	if auth.InstallationID == 0 {
		return nil, nil, fmt.Errorf("github: AppAuth.InstallationID must be non-zero")
	}
	if len(auth.PrivateKeyPEM) == 0 {
		return nil, nil, fmt.Errorf("github: AppAuth.PrivateKeyPEM must be non-empty")
	}
	owner, repo, err := parseFullName(fullName)
	if err != nil {
		return nil, nil, err
	}
	itr, err := ghinstallation.New(http.DefaultTransport, auth.AppID, auth.InstallationID, auth.PrivateKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("github: ghinstallation: %w", err)
	}
	httpClient := &http.Client{Transport: itr}
	cli := &realClient{
		c:     gh.NewClient(httpClient),
		owner: owner,
		repo:  repo,
	}
	return cli, itr, nil
}
