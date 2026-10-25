package executor

// The executor's workspace files, read-only, over MCP resources: what a
// session reads of the project where the project is (AGENTS.md,
// .dax/skills, .dax/config.json), not on its own machine.
//
// The resource template, its bounds and the checks on both sides are
// agentworkspace's mcpworkspace (wire.FilesURITemplate,
// agentworkspace:///{op}{?path}): the executor serves its workspace's FS
// with mcpworkspace.ServeFiles, and the session reads it with
// mcpworkspace.NewFS over the Remote's current connection, read again
// once on a new one when the connection was lost under it.

import (
	"io/fs"
	"time"

	"github.com/ChristopherDavenport/agentworkspace/mcpworkspace"
)

// filesTimeout bounds one request for the workspace's files. A request
// that takes longer fails as a read error does. A var for the tests.
var filesTimeout = 30 * time.Second

// filesOf is the files src reads, under the executor's name.
func filesOf(src mcpworkspace.Source) fs.FS {
	return mcpworkspace.NewFS(src, mcpworkspace.WithTimeout(filesTimeout), mcpworkspace.WithName("executor"))
}
