# Swarms

Swarms coordinate multiple polecats working on related tasks from a shared base commit.

## Concept

A swarm is a coordinated multi-agent work unit. When you have an epic with multiple
independent tasks, a swarm lets you:

1. **Parallelize** - Multiple polecats work simultaneously
2. **Isolate** - Each worker branches from the same base commit
3. **Integrate** - All work merges to an integration branch before landing
4. **Coordinate** - Task dispatch respects dependencies

```
                      epic (tasks)
                          │
                          ▼
┌────────────────────────────────────────────┐
│                   SWARM                    │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐   │
│  │ Polecat  │ │ Polecat  │ │ Polecat  │   │
│  │  Toast   │ │   Nux    │ │ Capable  │   │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘   │
│       │            │            │         │
│       ▼            ▼            ▼         │
│  ┌──────────────────────────────────────┐ │
│  │        integration/<epic>            │ │
│  └───────────────────┬──────────────────┘ │
└──────────────────────┼────────────────────┘
                       │
                       ▼ land
                    main
```

## Swarm Lifecycle

| State | Description |
|-------|-------------|
| `created` | Swarm set up, not yet started |
| `active` | Workers actively executing tasks |
| `merging` | All work done, integration in progress |
| `landed` | Successfully merged to main |
| `cancelled` | Swarm aborted |
| `failed` | Swarm failed and cannot recover |

## Commands

### Create a Swarm

```bash
# Create swarm from existing epic
gt swarm create gastown --epic gt-abc --worker Toast --worker Nux

# Create and start immediately
gt swarm create gastown --epic gt-abc --worker Toast --start

# Specify target branch (defaults to main)
gt swarm create gastown --epic gt-abc --worker Toast --target develop
```

The epic should already exist in beads with child tasks. The swarm will track
which tasks are ready, in-progress, and complete.

### Start a Swarm

```bash
# Start a previously created swarm
gt swarm start gt-abc
```

This transitions the swarm from `created` to `active` and begins dispatching
tasks to workers.

### Check Swarm Status

```bash
# Human-readable status
gt swarm status gt-abc

# JSON output
gt swarm status gt-abc --json
```

Shows:
- Swarm metadata (epic, rig, target branch)
- Ready front (tasks with no blockers)
- Active tasks (in-progress with assignees)
- Blocked tasks (waiting on dependencies)
- Completed tasks

### List Swarms

```bash
# All swarms across all rigs
gt swarm list

# Swarms in specific rig
gt swarm list gastown

# Filter by status
gt swarm list --status=active
gt swarm list gastown --status=landed

# JSON output
gt swarm list --json
```

### Dispatch Tasks

```bash
# Auto-dispatch next ready task to idle polecat
gt swarm dispatch gt-abc

# Dispatch in specific rig
gt swarm dispatch gt-abc --rig gastown
```

Finds the first unassigned ready task and assigns it to an available polecat.
Uses `gt sling` internally.

### Land a Swarm

```bash
# Manually land completed swarm
gt swarm land gt-abc
```

This:
1. Verifies all tasks are complete
2. Stops any running polecat sessions
3. Audits git state (checks for uncommitted/unpushed code)
4. Merges integration branch to target (main)
5. Cleans up branches
6. Closes the epic in beads

**Note**: Normally the Refinery handles landing automatically.

### Cancel a Swarm

```bash
gt swarm cancel gt-abc
```

Marks the swarm as cancelled. Does not automatically stop sessions or clean up
branches - use `gt swarm land` for full cleanup if needed.

## How It Works

### Beads Integration

Swarms are built on beads molecules:
- The epic is marked as `mol_type=swarm`
- Tasks are child issues of the epic
- Task dependencies are beads dependencies
- State is queried from beads, not cached

### Task Flow

1. **Ready front**: Tasks with no uncompleted dependencies are "ready"
2. **Dispatch**: `gt swarm dispatch` or `gt sling` assigns ready tasks
3. **Execution**: Polecat works the task, commits, signals `gt done`
4. **Merge**: Refinery merges to integration branch
5. **Next**: When dependencies clear, new tasks become ready

### Integration Branch

Each swarm has an integration branch: `swarm/<epic-id>`

- Created from the base commit when swarm starts
- Each completed task merges here
- Avoids merge conflicts from landing directly to main
- Final landing merges integration → main

### Git Safety

Before landing, the swarm manager audits each worker's git state:

| Check | Risk |
|-------|------|
| Uncommitted changes | Code loss if not in .beads/ |
| Unpushed commits | Code not in remote |
| Stashes | Forgotten work |

If code is at risk, landing blocks and notifies Mayor.

## Swarm vs Single Assignment

| Scenario | Use |
|----------|-----|
| Single task, one polecat | `gt sling` |
| Multiple independent tasks | `gt swarm create` |
| Sequential dependent tasks | Molecule (not swarm) |
| Large feature with subtasks | Swarm with dependencies |

## Example Workflow

```bash
# 1. Create epic with tasks in beads
bd create --type=epic --title="Add authentication" --id gt-auth
bd create --title="Add login form" --parent gt-auth
bd create --title="Add session management" --parent gt-auth
bd create --title="Add logout flow" --parent gt-auth

# 2. Create swarm
gt swarm create gastown --epic gt-auth --worker Toast --worker Nux --start

# 3. Monitor progress
gt swarm status gt-auth

# 4. Dispatch more as tasks complete
gt swarm dispatch gt-auth

# 5. Land when complete
gt swarm land gt-auth
```

## Troubleshooting

| Problem | Solution |
|---------|----------|
| "No ready tasks" | Check dependencies with `bd show <epic>` |
| "No idle polecats" | Create more with `gt polecat create` |
| "Code at risk" | Workers need to commit/push before landing |
| "Swarm not found" | Epic may not be marked `mol_type=swarm` |

## See Also

- [Molecules](molecules.md) - Workflow templates
- [Propulsion Principle](propulsion-principle.md) - Worker execution model
- [Mail Protocol](mail-protocol.md) - Agent communication
