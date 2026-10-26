#!/bin/bash
set -euo pipefail
curl -fsSL https://claude.ai/install.sh | bash

# initial claude setup to pickup login state from .claude/ and do not prompt user with same questions for each sandbox
printf '{
  "bypassPermissionsModeAccepted": true,
  "hasCompletedOnboarding": true,
  "projects": {
    "/": {
      "hasTrustDialogAccepted": true
    }
  }
}' > "${HOME}/.claude.json"

# Claude Code ignores trust entries above a project's git root, so the "/" entry above no longer
# stops its trust dialog; the sandbox flag does. command.json sets it for the exec'd agent, this
# covers claude started from a shell (`hole start -d`, hooks).
echo 'export CLAUDE_CODE_SANDBOXED=1' >> "${HOME}/.bash_env"
