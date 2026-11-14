// Package skills embeds the agent skill shipped with rov, so the binary can
// install the version that matches it (`rov skill install`).
package skills

import _ "embed"

// Rov is skills/rov/SKILL.md.
//
//go:embed rov/SKILL.md
var Rov []byte
