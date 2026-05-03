//go:build tools

// Package tools holds blank imports of dependencies used by other
// internal/* packages. The `tools` build tag excludes this file from
// regular builds. Its purpose is to keep go.mod and go.sum populated so
// parallel package implementations can rely on `import` working without
// running `go get` themselves (which would race on go.sum).
//
// Once every dep is imported by a real package, this file can be deleted.
package tools

import (
	_ "github.com/bradleyfalzon/ghinstallation/v2"
	_ "github.com/google/go-github/v68/github"
	_ "golang.org/x/oauth2"
	_ "gopkg.in/yaml.v3"
)
