package grafana

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type SemConvRegistry struct {
	Groups []SemConvGroup `yaml:"groups"`
}

type SemConvGroup struct {
	ID         string             `yaml:"id"`
	Type       string             `yaml:"type"`
	Prefix     string             `yaml:"prefix"`
	MetricName string             `yaml:"metric_name"`
	Instrument string             `yaml:"instrument"`
	Unit       string             `yaml:"unit"`
	Brief      string             `yaml:"brief"`
	Attributes []SemConvAttribute `yaml:"attributes"`
}

type SemConvAttribute struct {
	ID   string `yaml:"id"`
	Ref  string `yaml:"ref"`
	Type string `yaml:"type"`
}

type DiscoverTelemetryRegistryReq struct {
	Path string `json:"path" jsonschema:"Path to the directory containing Weaver OpenTelemetry YAML files. Defaults to '.'"`
}

type MetricInfo struct {
	Name       string   `json:"name"`
	Instrument string   `json:"instrument"`
	Unit       string   `json:"unit"`
	Brief      string   `json:"brief"`
	Attributes []string `json:"attributes,omitempty"`
}

type DiscoverTelemetryRegistryRes struct {
	Metrics []MetricInfo `json:"metrics"`
}

func discoverTelemetryRegistryHandler() mcp.ToolHandlerFor[DiscoverTelemetryRegistryReq, DiscoverTelemetryRegistryRes] {
	return func(_ context.Context, _ *mcp.CallToolRequest, args DiscoverTelemetryRegistryReq) (*mcp.CallToolResult, DiscoverTelemetryRegistryRes, error) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, DiscoverTelemetryRegistryRes{}, fmt.Errorf("failed to get current working directory: %w", err)
		}

		metrics, err := discoverTelemetryRegistry(cwd, args.Path)
		if err != nil {
			return nil, DiscoverTelemetryRegistryRes{}, err
		}
		return nil, DiscoverTelemetryRegistryRes{Metrics: metrics}, nil
	}
}

func discoverTelemetryRegistry(cwd, searchPath string) ([]MetricInfo, error) {
	if searchPath == "" {
		searchPath = "."
	}

	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(cwd, searchPath)
	}
	searchPath, err = filepath.Abs(searchPath)
	if err != nil {
		return nil, fmt.Errorf("resolve search path: %w", err)
	}
	rel, err := filepath.Rel(cwd, searchPath)
	if err != nil {
		return nil, fmt.Errorf("check search path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path must be inside the working directory")
	}

	metrics, err := discoverTelemetryRegistryFS(os.DirFS(searchPath))
	if err != nil {
		return nil, fmt.Errorf("error walking path: %w", err)
	}

	return metrics, nil
}

func discoverTelemetryRegistryFS(fsys fs.FS) ([]MetricInfo, error) {
	metrics := make([]MetricInfo, 0)

	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip standard ignore dirs below the root search directory.
		if d.IsDir() {
			name := d.Name()
			if path != "." && (name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(d.Name(), ".yaml") && !strings.HasSuffix(d.Name(), ".yml") {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			// skip unreadable files
			return nil
		}

		var reg SemConvRegistry
		if err := yaml.Unmarshal(data, &reg); err != nil {
			// not a valid registry file, skip
			return nil
		}

		for _, group := range reg.Groups {
			if group.Type != "metric" {
				continue
			}
			var attrNames []string
			for _, attr := range group.Attributes {
				name := attr.ID
				if name == "" {
					name = attr.Ref
				}
				if name != "" {
					attrNames = append(attrNames, name)
				}
			}

			metricName := group.MetricName
			if metricName == "" {
				metricName = group.ID
			}

			metrics = append(metrics, MetricInfo{
				Name:       metricName,
				Instrument: group.Instrument,
				Unit:       group.Unit,
				Brief:      strings.TrimSpace(group.Brief),
				Attributes: attrNames,
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return metrics, nil
}
