package raccoon

import (
	"errors"
	"os"
)

// Errors that callers must translate into account state.
var (
	// ErrNeedsLogin marks a credential that cannot be repaired by a refresh:
	// the upstream rejected it and (for IAM) there is no refresh grant at all.
	// The pool parks the account and the panel asks the operator to sign in.
	ErrNeedsLogin = errors.New("小浣熊登录态需要重新登录")
	// ErrNoRefreshToken marks a non-IAM account whose refresh token is missing
	// (older imports, hand-edited files).
	ErrNoRefreshToken = errors.New("小浣熊账号缺少 refreshToken，无法续期")
	// ErrNoCredential marks a record that carries no access token at all.
	ErrNoCredential = errors.New("小浣熊账号缺少 accessToken")
)

// getenv is a variable so tests can stub the environment without touching the
// process environment.
var getenv = os.Getenv
