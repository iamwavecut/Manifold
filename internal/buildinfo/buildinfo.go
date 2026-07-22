package buildinfo

import "slices"

const (
	APIMajor         = 1
	ProtocolRevision = 2
)

var protocolFeatures = []string{
	"documents.folder_path",
	"memory.remember.v1",
	"retrieval.scope_glob",
	"retrieval.source_types",
	"tree.glob",
}

type Info struct {
	Service          string   `json:"service"`
	Version          string   `json:"version"`
	Commit           string   `json:"commit"`
	APIMajor         int      `json:"api_major"`
	ProtocolRevision int      `json:"protocol_revision"`
	Features         []string `json:"features"`
}

func New(version, commit string) Info {
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	return Info{
		Service:          "manifold",
		Version:          version,
		Commit:           commit,
		APIMajor:         APIMajor,
		ProtocolRevision: ProtocolRevision,
		Features:         slices.Clone(protocolFeatures),
	}
}
