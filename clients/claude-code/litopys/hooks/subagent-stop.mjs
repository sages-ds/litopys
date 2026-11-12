#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { subagentStop } from "../lib/hooks.mjs";

try {
  process.stdout.write(subagentStop(await readStdin()));
} catch {
  // the sub-agent ends anyway
}
