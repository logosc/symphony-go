package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

func TestNewAppClient_RejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	validPEM := generateTestPEM(t)

	cases := []struct {
		name          string
		auth          AppAuth
		full          string
		wantErrSubstr string
	}{
		{
			name:          "missing app id",
			auth:          AppAuth{InstallationID: 1, PrivateKeyPEM: validPEM},
			full:          "owner/repo",
			wantErrSubstr: "AppID",
		},
		{
			name:          "missing installation id",
			auth:          AppAuth{AppID: 1, PrivateKeyPEM: validPEM},
			full:          "owner/repo",
			wantErrSubstr: "InstallationID",
		},
		{
			name:          "missing private key",
			auth:          AppAuth{AppID: 1, InstallationID: 1},
			full:          "owner/repo",
			wantErrSubstr: "PrivateKeyPEM",
		},
		{
			name:          "bad full name",
			auth:          AppAuth{AppID: 1, InstallationID: 1, PrivateKeyPEM: validPEM},
			full:          "no-slash",
			wantErrSubstr: "full_name",
		},
		{
			name:          "malformed PEM",
			auth:          AppAuth{AppID: 1, InstallationID: 1, PrivateKeyPEM: []byte("not a pem")},
			full:          "owner/repo",
			wantErrSubstr: "ghinstallation",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewAppClient(context.Background(), tc.auth, tc.full)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("err=%q, want substring %q", err, tc.wantErrSubstr)
			}
		})
	}
}

func TestNewAppClient_AcceptsValidInputs(t *testing.T) {
	t.Parallel()
	pem := generateTestPEM(t)
	cli, err := NewAppClient(context.Background(), AppAuth{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPEM:  pem,
	}, "logosc/symphony-go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cli == nil {
		t.Fatal("nil client")
	}
	// The returned Client satisfies the interface (no API call needed).
	var _ Client = cli
}

// generateTestPEM produces a fresh 2048-bit RSA key encoded as a PKCS#1
// "RSA PRIVATE KEY" PEM block, suitable for handing to ghinstallation.
func generateTestPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}
