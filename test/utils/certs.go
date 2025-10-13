package utils

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// validatePath performs basic validation on file paths to prevent command injection
func validatePath(path string) error {
	// Use filepath.Clean to sanitize the path
	cleanPath := filepath.Clean(path)

	// Ensure the path doesn't contain shell metacharacters
	if strings.ContainsAny(cleanPath, ";|&$`(){}[]<>") {
		return fmt.Errorf("path contains invalid characters: %s", path)
	}

	// Ensure it's not an absolute path to prevent access to system directories
	if filepath.IsAbs(cleanPath) && !strings.HasPrefix(cleanPath, "/tmp/") {
		return fmt.Errorf("path must be relative or in /tmp: %s", path)
	}

	return nil
}

func GenerateCertificates(path string, caPath string) error {
	var err error
	fmt.Println("====Generating TLS Certificates")

	// Validate paths to prevent command injection
	if err := validatePath(path); err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}
	if err := validatePath(caPath); err != nil {
		return fmt.Errorf("invalid caPath: %w", err)
	}

	// Use safer approach with individual commands instead of shell execution
	commands := [][]string{
		{"openssl", "genpkey", "-algorithm", "RSA", "-out", filepath.Join(path, "tls.key")},
		{"openssl", "req", "-new", "-key", filepath.Join(path, "tls.key"), "-config", filepath.Join(path, "server.cnf"), "-out", filepath.Join(path, "tls.csr")},
		{"openssl", "x509", "-req", "-CA", filepath.Join(caPath, "cacert.pem"), "-CAkey", filepath.Join(caPath, "ca-private-key.pem"), "-CAcreateserial", "-CAserial", filepath.Join(path, "cacert.srl"), "-in", filepath.Join(path, "tls.csr"), "-out", filepath.Join(path, "tls.crt"), "-days", "365"},
	}

	for _, cmdArgs := range commands {
		// #nosec G204 - Command arguments are constructed from validated paths
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		err = cmd.Run()
		if err != nil {
			return fmt.Errorf("failed to execute command %v: %w", cmdArgs, err)
		}
	}
	return err
}

func GenerateCACertificate(caPath string) error {
	var err error
	fmt.Println("====Generating CA Certificates")

	// Validate path to prevent command injection
	if err := validatePath(caPath); err != nil {
		return fmt.Errorf("invalid caPath: %w", err)
	}

	// Use safer approach with individual commands instead of shell execution
	commands := [][]string{
		{"pwd"},
		{"openssl", "genrsa", "-out", filepath.Join(caPath, "ca-private-key.pem"), "2048"},
		{"openssl", "req", "-new", "-x509", "-days", "3650", "-key", filepath.Join(caPath, "ca-private-key.pem"), "-out", filepath.Join(caPath, "cacert.pem"), "-subj", "/CN=TlsTest/C=US/ST=California/L=RedwoodCity/O=Progress/OU=MarkLogic"},
	}

	for _, cmdArgs := range commands {
		fmt.Println(strings.Join(cmdArgs, " "))
		// #nosec G204 - Command arguments are constructed from validated paths
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		err = cmd.Run()
		if err != nil {
			fmt.Println(err)
			return fmt.Errorf("failed to execute command %v: %w", cmdArgs, err)
		}
	}
	return nil
}
