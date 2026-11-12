package server

import (
	"sync"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/api"
)

// clients remembers the clientInfo of the last initialize per token. The facade is stateless: every request
// gets a temporary session with default parameters, so what a client said in initialize ("codex-mcp-client/0.160.0")
// never reaches its tool calls by itself. Declarative, like X-Litopys-Client: it names the client for whoami and the
// audit, and decides nothing. Per process and per token: an OAuth client is known again after its next initialize
// once its access token is refreshed; two clients on one token (claude.ai web and mobile) — the last one wins.
type clients struct {
	mu sync.Mutex
	m  map[uuid.UUID]string
}

// clientsMax bounds the memory of one process; when full, it starts over (the next initialize refills it).
const clientsMax = 4096

func (c *clients) remember(token uuid.UUID, label string) {
	if label == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= clientsMax {
		c.m = map[uuid.UUID]string{}
	}
	c.m[token] = label
}

func (c *clients) of(token uuid.UUID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[token]
}

// ClientLabel: "name/version" of an MCP clientInfo, "" when there is none — the same label as the native API's.
var ClientLabel = api.ClientLabel
