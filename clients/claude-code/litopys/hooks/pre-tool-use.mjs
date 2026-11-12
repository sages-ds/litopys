#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { preToolUse } from "../lib/hooks.mjs";

try {
  process.stdout.write(preToolUse(await readStdin()));
} catch {
  // only a detected address conflict blocks a tool: a failure here must not
}
