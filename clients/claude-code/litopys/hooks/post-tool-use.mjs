#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { postToolUse } from "../lib/hooks.mjs";

try {
  process.stdout.write(await postToolUse(await readStdin()));
} catch {
  // counting writes and touching projects are conveniences: never break the tool call
}
