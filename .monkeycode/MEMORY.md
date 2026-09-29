# User Instruction Memory

This file records user instructions, preferences, and teachings for reference in future interactions.

## Format

### User Instruction Entry
[User Instruction Summary]
- Date: [YYYY-MM-DD]
- Context: [Mentioned scenario or time]
- Instructions:
  - [Content of user teaching or instruction, described line by line]

### Project Knowledge Entry
[Project Knowledge Summary]
- Date: [YYYY-MM-DD]
- Context: Discovered by Agent while performing [specific task description]
- Category: [Operations & Deployment|Build Methods|Testing Methods|Troubleshooting & Debugging|Workflow & Collaboration|Environment Configuration]
- Instructions:
  - [Specific knowledge points, described line by line]

## Deduplication Strategy
- Before adding a new entry, check for similar or identical instructions.
- If a duplicate is found, skip the new entry or merge it with the existing one.
- When merging, update the context or date information.
- This helps avoid redundant entries and keeps the memory file tidy.

## Entries

[Project Knowledge Summary]
- Date: 2026-09-29
- Context: Discovered by Agent while restructuring the mrp.bat device menus to show unified status lines for Android (adb) and HarmonyOS (hdc)
- Category: Troubleshooting & Debugging
- Instructions:
  - HarmonyOS hdc has no verified global-HTTP-proxy *query* command. The set path is `hdc shell network-cfg set http_proxy <host:port>`, but no equivalent `get` was confirmable from official docs. The user explicitly chose to leave hdc proxy status as `Unknown` rather than wire in a guessed command. Do not add hdc proxy probing with an unverified command; only adb is probed (`adb shell settings get global http_proxy`, compared to `127.0.0.1:<PORT>`).
  - `adb shell` and `hdc shell` both merge device-side stderr into the host-side stdout, and exit codes are unreliable. Remote file-existence checks must therefore use a command that emits nothing on failure (`test -f %1 && echo Y`, judged by temp-file size) — never `ls %1`, whose error line leaks into stdout and makes the path always look present. `pidof <bin>` is safe because it prints nothing when no process matches.
