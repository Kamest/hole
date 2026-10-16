#!/bin/bash
set -euo pipefail

# Updates the CLI only, under the Node install-user.sh pinned — command.json and version.json name
# that interpreter by absolute path. nvm is not loaded: its default may be another Node (a setup
# hook installing one makes it the default), whose npm would update a CLI the agent never runs.
# PATH is extended for this process only, so package install scripts that run `node` get the
# pinned one too.
node_bin="${HOME}/.nvm/versions/node/v22.22.2/bin"
export PATH="${node_bin}:${PATH}"

"${node_bin}/npm" install -g @openai/codex@latest
"${node_bin}/npm" cache clean --force
