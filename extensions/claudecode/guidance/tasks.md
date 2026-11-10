## Work that outlives your turn (`claude-code.task.*`)

Everything you start yourself ends with your turn: background shell commands, `nohup` or `&`, Monitors and Workflows are all stopped when the turn ends, and nothing tells you they were. For anything that should keep running after your turn (a long build, a test suite, a data job, a headless `claude -p` run), start it with `claude-code.task.start` instead, then park on it.

1. `claude-code.task.start` with `{"command": "…", "name": "suite"}`. It runs with `/bin/sh -c` in your working directory with your environment, but without the toolbelt: a task can't call Open Conduit. Its output goes to the log the call returns. It is stopped as `timed_out` after `timeout_minutes` (default 240).
2. Make the command write anything you need afterwards to a file in your working directory or its log; read them when you are resumed.
3. End your turn with `claude-code.task.wait` (`{}` waits for every running task; `{"tasks": ["suite"]}` for named ones; add `until` for the latest you want to be woken). It is the last call of the turn. You are resumed when a task ends, with its outcome (`succeeded`, `failed`, `stopped`, `timed_out` or `lost`), exit code, last line of output and log path. Other news that arrives meanwhile is handed to you then too.

`claude-code.task.list` shows this Work's tasks and `claude-code.task.stop` stops one. Give the same `request_id` when you retry a start, so a task never starts twice. A task that is still running when the Work finishes is stopped.
