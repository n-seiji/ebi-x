//go:build !darwin

package github

import "errors"

func readKeychain(string) ([]byte, error) {
	return nil, errors.New("the Keychain is only available on macOS")
}
