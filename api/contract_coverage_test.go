package api

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This deliberately checks the published contract against registration sites,
// rather than a separately maintained list of supposedly implemented routes.
func TestOpenAPIPathsHaveRegisteredHandlers(t *testing.T) {
	contract, err := openAPIOperations(filepath.Join("openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	registered, err := registeredOperations(filepath.Join("..", "services"))
	if err != nil {
		t.Fatal(err)
	}
	for operation, paths := range contract {
		for _, path := range paths {
			if !covers(registered, operation, path) {
				t.Errorf("OpenAPI operation %s %s has no registered handler", operation, path)
			}
		}
	}
}

func openAPIOperations(name string) (map[string][]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	result := make(map[string][]string)
	var path string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "  /") && strings.HasSuffix(line, ":") {
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}
		if path == "" || len(line) < 5 || line[:4] != "    " || strings.HasPrefix(line, "      ") || !strings.HasSuffix(line, ":") {
			continue
		}
		method := strings.TrimSuffix(strings.TrimSpace(line), ":")
		if method == "get" || method == "post" || method == "put" || method == "patch" || method == "delete" {
			result[strings.ToUpper(method)] = append(result[strings.ToUpper(method)], path)
		}
	}
	return result, scanner.Err()
}

func registeredOperations(root string) (map[string][]string, error) {
	result := make(map[string][]string)
	err := filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(name) != "main.go" {
			return err
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()
			marker := "HandleFunc(\""
			start := strings.Index(line, marker)
			if start < 0 {
				continue
			}
			value := line[start+len(marker):]
			end := strings.IndexByte(value, '"')
			if end < 0 {
				continue
			}
			value = value[:end]
			method := "*"
			path := value
			if fields := strings.SplitN(value, " ", 2); len(fields) == 2 {
				method, path = fields[0], fields[1]
			}
			result[method] = append(result[method], path)
		}
		return scanner.Err()
	})
	return result, err
}

func covers(registered map[string][]string, method, path string) bool {
	for _, candidate := range append(registered[method], registered["*"]...) {
		if candidate == path || strings.HasSuffix(candidate, "/") && strings.HasPrefix(path, candidate) {
			return true
		}
	}
	return false
}
