# ADR-0001: Existing agent as controller; independent durable service

Status: accepted.

The operator should remain in the harness they prefer. The current conversation
adopts the controller role via explicitly invoked Council tools. A separate fresh
session from the same harness is its independent contributor.

A Go service owns execution and durable state. CLI, MCP bridge and any future UI
are clients. MCP is not an obligatory interception hook. No native-specific plugin
owns the cross-harness process; plugins are optional wake-up/status conveniences.

Closing the controller client is not a cancellation request. Bounded active turns
may finish and park. Autonomous decision-making after controller exit would require
an explicitly authorized service-managed controller; it is not the initial mode.

Rejected defaults: controller hard-wired to OpenCode; losing state with a terminal;
fresh conversations for every phase; silently choosing three contributors; a UI
framework or Takumi deployment required before the first live run.
