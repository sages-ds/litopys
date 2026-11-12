#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { stop } from "../lib/hooks.mjs";

try {
  process.stdout.write(stop(await readStdin()));
} catch {
  // a reminder must never keep the model from stopping
}
