package watchdog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcp_contract_test.go locks the externally visible management contract:
// tool names, titles, descriptions, annotations, and input schemas exactly as
// a client sees them over tools/list, plus the app resource. A refactor must
// not change this output; an intentional contract change updates the golden
// file and docs/api/management-tools.md together.

type contractTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

type contractListing struct {
	ServerName    string         `json:"server_name"`
	ServerVersion string         `json:"server_version"`
	Tools         []contractTool `json:"tools"`
	Resources     []contractRes  `json:"resources"`
}

type contractRes struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MIMEType    string          `json:"mimeType,omitempty"`
	Meta        json.RawMessage `json:"_meta,omitempty"`
}

func rawOrNull(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" {
		return nil
	}
	return data
}

func listManagementContract(t *testing.T) contractListing {
	t.Helper()
	ctx := context.Background()

	server := newManagementMCPServer(newManagementService(t.TempDir()))
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test"}, nil)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()

	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	toolsResult, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	resourcesResult, err := clientSession.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}

	listing := contractListing{
		ServerName:    "codex-auto-retry",
		ServerVersion: appVersion,
	}
	for _, tool := range toolsResult.Tools {
		annotations, err := json.Marshal(tool.Annotations)
		if err != nil {
			t.Fatalf("marshal annotations for %s: %v", tool.Name, err)
		}
		meta, err := json.Marshal(tool.Meta)
		if err != nil {
			t.Fatalf("marshal meta for %s: %v", tool.Name, err)
		}
		listing.Tools = append(listing.Tools, contractTool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			Annotations: annotations,
			InputSchema: rawOrNull(tool.InputSchema),
			Meta:        meta,
		})
	}
	sort.Slice(listing.Tools, func(i, j int) bool { return listing.Tools[i].Name < listing.Tools[j].Name })
	for _, resource := range resourcesResult.Resources {
		meta, err := json.Marshal(resource.Meta)
		if err != nil {
			t.Fatalf("marshal meta for %s: %v", resource.URI, err)
		}
		listing.Resources = append(listing.Resources, contractRes{
			URI:         resource.URI,
			Name:        resource.Name,
			Title:       resource.Title,
			Description: resource.Description,
			MIMEType:    resource.MIMEType,
			Meta:        meta,
		})
	}
	return listing
}

func TestManagementMCPContract(t *testing.T) {
	listing := listManagementContract(t)

	actual, err := json.MarshalIndent(listing, "", "  ")
	if err != nil {
		t.Fatalf("marshal listing: %v", err)
	}
	actual = append(actual, '\n')

	goldenPath := filepath.Join("testdata", "mcp_tools.golden.json")
	if os.Getenv("UPDATE_MCP_CONTRACT") == "1" {
		if err := os.WriteFile(goldenPath, actual, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_MCP_CONTRACT=1 go test to regenerate): %v", err)
	}
	if string(actual) != string(expected) {
		t.Fatalf("management MCP contract drifted from testdata/mcp_tools.golden.json; if intentional, run UPDATE_MCP_CONTRACT=1 go test -run TestManagementMCPContract and update docs/api/management-tools.md")
	}
}
