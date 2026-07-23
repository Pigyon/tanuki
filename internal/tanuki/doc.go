// Package tanuki implements a transparent anonymization proxy for AI-assisted
// security testing. It sits between a local Claude Code agent and the Anthropic
// API and rewrites every target-identifying value in both directions: real to
// fiction outbound (the model sees only a localhost project) and fiction back
// to real inbound (local tools act on the real target).
package tanuki
