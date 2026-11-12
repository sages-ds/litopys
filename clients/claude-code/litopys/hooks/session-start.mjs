#!/usr/bin/env node
import { readStdin } from "../lib/core.mjs";
import { sessionStart } from "../lib/hooks.mjs";

process.stdout.write(await sessionStart(await readStdin()));
