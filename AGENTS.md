# AGENTS.md

This file contains guidelines and commands for agentic coding agents working in this repository.

## Project Overview

This is a sandbox-based guessing game system with three main components:
- **Controller** (Go): Orchestrates the game, manages sandboxes and communication
- **Judge** (C++): Validates guesses and reports results via fd=3
- **Player** (Go): Implements binary search guessing strategy

The system uses Linux namespaces and pivot_root to create isolated sandboxes for judge and player processes.

## Build Commands

### Go Components
```bash
# Build controller
go build -o cmd/ctr cmd/contr/main.go

# Build player binary
go build -o cmd/player/player cmd/player/main.go

# Build all Go components
go build ./cmd/...
```

### C++ Component
```bash
# Build judge binary
g++ -o cmd/rootfs_judge/bin/judge cmd/judger/main.cpp

# Or with Make (if available)
make judge
```

### Testing
```bash
# Run Go tests
go test ./...

# Run specific test
go test -run TestName ./path/to/package

# Test with verbose output
go test -v ./...
```

### Linting and Formatting
```bash
# Format Go code
go fmt ./...

# Run Go vet
go vet ./...

# Run golint (if installed)
golint ./...

# Format C++ code (if clang-format available)
clang-format -i cmd/judger/main.cpp
```

## Code Style Guidelines

### Go Code Style

#### Imports
- Group imports in three sections: standard library, third-party, local packages
- Use blank line between groups
- Prefer explicit imports over dot imports

```go
import (
    "bufio"
    "flag"
    "fmt"
    "log"
    "os"
    "syscall"
    "time"
)
```

#### Naming Conventions
- Use `camelCase` for variables and functions
- Use `PascalCase` for exported types and functions
- Use `UPPER_SNAKE_CASE` for constants
- Use descriptive names, avoid abbreviations unless common

```go
const nobodyUID = 65534
const nobodyGID = 65534

type SandboxConfig struct {
    JudgeRootfs  string
    JudgeCmd     string
    PlayerRootfs string
    PlayerCmd    string
    TimeoutMS    int
}

func spawnSandbox(cmdPath, rootfs string, stdin, stdout *os.File, extraFiles []*os.File) (*exec.Cmd, error) {
    // implementation
}
```

#### Error Handling
- Use explicit error returns, not panic for expected errors
- Use helper functions like `must()` for fatal errors in main/init
- Check errors immediately after function calls

```go
func must(err error) {
    if err != nil {
        log.Fatal(err)
    }
}

func someFunction() error {
    if err := doSomething(); err != nil {
        return fmt.Errorf("failed to do something: %w", err)
    }
    return nil
}
```

#### Comments
- Use Chinese comments for user-facing messages and domain-specific concepts
- Use English comments for technical implementation details
- Keep comments concise and focused on "why" not "what"

```go
// Result 表示 judge 向 controller 汇报的结果
type Result struct {
    Status string `json:"status"`
    Reason string `json:"reason,omitempty"`
}

// helper
func must(err error) {
    if err != nil {
        log.Fatal(err)
    }
}
```

### C++ Code Style

#### Includes
- Group includes: standard library, system headers, local headers
- Use angle brackets for system headers, quotes for local headers

```cpp
#include <iostream>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
```

#### Naming Conventions
- Use `snake_case` for variables and functions
- Use `PascalCase` for classes/structs
- Use `UPPER_SNAKE_CASE` for constants

```cpp
void report(const char* s) {
    write(3, s, strlen(s));
    write(3, "\n", 1);
}

int main() {
    const int secret = 731;
    int x;
    // implementation
}
```

#### Error Handling
- Use system calls directly for low-level operations
- Report errors via fd=3 using JSON format
- Check return values of system calls

```cpp
void report(const char* s) {
    write(3, s, strlen(s));
    write(3, "\n", 1);
}

if (!(std::cin >> x)) {
    report("{\"status\":\"RE\",\"reason\":\"bad input\"}");
    return 0;
}
```

## Architecture Guidelines

### Sandbox Management
- Use Linux namespaces: CLONE_NEWNS, CLONE_NEWPID, CLONE_NEWUTS, CLONE_NEWIPC
- Implement pivot_root for filesystem isolation
- Drop privileges to nobody (UID/GID 65534)
- Use environment variables for sandbox initialization

### Communication Protocol
- Judge ↔ Player: stdin/stdout with text responses
- Judge → Controller: fd=3 with JSON status reports
- Status formats: "AC" (Accepted), "WA" (Wrong Answer), "RE" (Runtime Error)

### Security Considerations
- Never run untrusted code outside sandbox
- Always drop privileges before executing user code
- Use timeouts to prevent infinite loops
- Validate all inputs and handle errors gracefully

## File Structure

```
cmd/
├── contr/           # Controller (Go)
│   └── main.go
├── player/          # Player binary (Go)
│   └── main.go
├── judger/          # Judge source (C++)
│   └── main.cpp
├── rootfs_judge/    # Judge sandbox rootfs
│   └── bin/
│       └── judge
├── rootfs_player/   # Player sandbox rootfs
│   └── bin/
│       └── player
└── ctr              # Controller binary
```

## Development Workflow

1. Make changes to source files
2. Build affected components
3. Test with sample inputs
4. Run linting/formatting tools
5. Verify sandbox isolation works correctly

## Testing Strategy

- Unit tests for individual components
- Integration tests for full game flow
- Security tests for sandbox isolation
- Performance tests for timeout handling