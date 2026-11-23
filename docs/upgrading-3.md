# Upgrading to 3.0.0

Breaking changes from 2.x, one row each. Check every row against your values and playbooks
before `helm upgrade`.

| 2.x | 3.0.0 | What to do |
|---|---|---|
| `mode: yolo` (global or playbook) pushes to the default branch | Removed: `mode: yolo` fails to load with `mode "yolo" was removed in 3.0.0` | Use `mode: full`; fixes arrive as PRs that need review |
