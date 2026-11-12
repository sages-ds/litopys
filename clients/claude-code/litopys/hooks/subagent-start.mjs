#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { subagentStart } from "../lib/hooks.mjs";

try {
  process.stdout.write(subagentStart(await readStdin()));
} catch {
  // the sub-agent starts anyway
}
