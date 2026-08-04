package database

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// RequireTLS rejects PostgreSQL URLs that can negotiate a plaintext connection.
func RequireTLS(databaseURL string) error {
	if strings.TrimSpace(databaseURL) == "" {
		return errors.New("DATABASE_URL is required")
	}
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("DATABASE_URL must be a valid postgres or postgresql URL")
	}
	if u.Hostname() == "" {
		return errors.New("DATABASE_URL must include a database hostname")
	}
	values, ok := u.Query()["sslmode"]
	if !ok || len(values) != 1 {
		return fmt.Errorf("DATABASE_URL must enforce TLS with sslmode=require, verify-ca, or verify-full")
	}
	switch strings.ToLower(values[0]) {
	case "require", "verify-ca", "verify-full":
		return nil
	default:
		return fmt.Errorf("DATABASE_URL must enforce TLS with sslmode=require, verify-ca, or verify-full")
	}
}
