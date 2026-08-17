# Aider rules bundle

Aider does not provide a plugin or MCP client API. Install `AIDER.md` as a
read-only instruction file via Aider's `--read` option or project workflow.
This entry is included so the local fleet is explicit; it is not claimed to
provide tools Aider cannot load.

No `/scos-recall`-style slash commands here either: Aider's slash commands
are a fixed built-in set (`/add`, `/ask`, `/commit`, …) with no supported
mechanism for registering new ones as of this writing. Don't add a
`commands/` or `workflows/` directory to this bundle on the assumption
other agents have one — verify against Aider's docs first if that changes.
