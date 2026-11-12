package api

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
)

// Canary of the channels: which of instructions, a tool description and a tool result reach the
// model of a client. For the listed actors only, the MCP surface carries markers:
//
//	CANARY-INSTR-HEAD-<t>  first line of instructions
//	CANARY-INSTR-CORE-<t>  right after InstructionsCore (~512 characters: what OpenAI clients weigh most)
//	CANARY-INSTR-TAIL-<t>  last line of instructions (Claude Code cuts at 2 KB)
//	CANARY-DESC-<t>        end of the description of system_whoami
//	CANARY-RES-<t>         field "canary" of the system_whoami result
//
// The procedure (evals/canary/README.md): ask the client to call system_whoami and repeat every CANARY line it was
// given, verbatim. Everyone else sees the normal surface.
type Canary struct {
	Token  string
	Actors map[uuid.UUID]bool
}

var canaryTokenRe = regexp.MustCompile(`^[A-Za-z0-9]{4,32}$`)

// CanaryFromEnv reads LITOPYS_CANARY (the token) and LITOPYS_CANARY_ACTORS (comma-separated actor ids). No token =
// no canary; a token without actors is refused, so a canary never reaches every user by mistake.
func CanaryFromEnv() (*Canary, error) {
	tok := strings.TrimSpace(os.Getenv("LITOPYS_CANARY"))
	if tok == "" {
		return nil, nil
	}
	if !canaryTokenRe.MatchString(tok) {
		return nil, errors.New("LITOPYS_CANARY: 4-32 letters or digits")
	}
	c := &Canary{Token: tok, Actors: map[uuid.UUID]bool{}}
	for _, s := range strings.Split(os.Getenv("LITOPYS_CANARY_ACTORS"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("LITOPYS_CANARY_ACTORS: %q is not an actor id", s)
		}
		c.Actors[id] = true
	}
	if len(c.Actors) == 0 {
		return nil, errors.New("LITOPYS_CANARY is set but LITOPYS_CANARY_ACTORS is empty: the canary is for listed actors only")
	}
	return c, nil
}

// For: does this principal get the canary surface.
func (c *Canary) For(p auth.Principal) bool { return c != nil && c.Actors[p.ActorID] }

func (c *Canary) marker(kind string) string { return "CANARY-" + kind + "-" + c.Token }

// instructions wraps the normal text with the three markers.
func (c *Canary) instructions() string {
	rest := strings.TrimPrefix(Instructions, InstructionsCore)
	return c.marker("INSTR-HEAD") + "\n" + InstructionsCore + "\n" + c.marker("INSTR-CORE") + rest + "\n" + c.marker("INSTR-TAIL")
}

func (c *Canary) description(d string) string { return d + " " + c.marker("DESC") }

// result adds the result marker to a system_whoami result.
func (c *Canary) result(res any) any {
	if m, ok := res.(map[string]any); ok {
		m["canary"] = c.marker("RES")
	}
	return res
}
