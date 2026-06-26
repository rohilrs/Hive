#!/usr/bin/env bash
# Bootstrap fixture git repos for integration tests. Idempotent.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURES="$SCRIPT_DIR/fixtures/repos"

mkdir -p "$FIXTURES"

# simple-go: minimal Go module with one package + one file
SIMPLE="$FIXTURES/simple-go"
if [ ! -d "$SIMPLE/.git" ]; then
  rm -rf "$SIMPLE"
  mkdir -p "$SIMPLE"
  cd "$SIMPLE"
  git init -q -b main
  git config user.name "Hive Test Fixture"
  git config user.email "fixture@hive.test"
  cat > go.mod <<EOF
module hive.test/simple-go

go 1.22
EOF
  mkdir -p pkg/greeter
  cat > pkg/greeter/greeter.go <<'EOF'
package greeter

// Hello returns a friendly greeting.
func Hello(name string) string {
    return "Hello, " + name + "!"
}
EOF
  cat > pkg/greeter/greeter_test.go <<'EOF'
package greeter

import "testing"

func TestHello(t *testing.T) {
    if got := Hello("world"); got != "Hello, world!" {
        t.Errorf("Hello(world) = %q", got)
    }
}
EOF
  git add -A
  GIT_COMMITTER_DATE="2020-01-01T00:00:00Z" GIT_AUTHOR_DATE="2020-01-01T00:00:00Z" \
    git commit -q -m "Initial commit"
fi

echo "fixtures ready: $FIXTURES"
