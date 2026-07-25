package snoozebot

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"golang.org/x/tools/txtar"
)

type osFS struct{}

func (osFS) Open(name string) (fs.File, error) {
	return os.Open(name)
}

func TestConfigTxtar(t *testing.T) {
	testdataDir := "testdata/config"
	entries, err := fs.ReadDir(osFS{}, testdataDir)
	if err != nil {
		t.Fatalf("failed to read testdata dir: %v", err)
	}

	var cases []string
	for _, d := range entries {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".txtar") {
			cases = append(cases, path.Join(testdataDir, d.Name()))
		}
	}
	sort.Strings(cases)

	for _, tc := range cases {
		tc := tc
		t.Run(strings.TrimSuffix(path.Base(tc), ".txtar"), func(t *testing.T) {
			raw, err := fs.ReadFile(osFS{}, tc)
			if err != nil {
				t.Fatalf("failed to read %s: %v", tc, err)
			}
			ar := txtar.Parse(raw)

			envVars := make(map[string]string)
			var expectedJSON []byte
			vfs := fstest.MapFS{}

			for _, f := range ar.Files {
				if f.Name == "env" {
					lines := strings.Split(string(f.Data), "\n")
					for _, line := range lines {
						line = strings.TrimSpace(line)
						if line == "" {
							continue
						}
						parts := strings.SplitN(line, "=", 2)
						if len(parts) == 2 {
							envVars[parts[0]] = parts[1]
						}
					}
				} else if f.Name == "expected.json" {
					expectedJSON = f.Data
				} else if strings.HasPrefix(f.Name, "fs/") {
					vfs[strings.TrimPrefix(f.Name, "fs/")] = &fstest.MapFile{Data: append([]byte(nil), f.Data...)}
				}
			}

			// Save old lookup functions and restore them later
			oldLookupEnv := lookupEnv
			oldReadFile := readFile
			defer func() {
				lookupEnv = oldLookupEnv
				readFile = oldReadFile
			}()

			// Mock environment variables
			lookupEnv = func(key string) (string, bool) {
				val, ok := envVars[key]
				return val, ok
			}

			// Mock file reader
			readFile = func(name string) ([]byte, error) {
				return fs.ReadFile(vfs, strings.TrimPrefix(name, "/"))
			}

			cfg := loadConfig()

			var expectedConfig Config
			err = json.Unmarshal(expectedJSON, &expectedConfig)
			if err != nil {
				t.Fatalf("failed to parse expected.json: %v", err)
			}

			assert.Equal(t, expectedConfig, cfg)
		})
	}
}
