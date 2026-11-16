#!/usr/bin/env node
// Install this wrapper with its adjacent source tree only after the attended rollout.
// Requires Node 24; never chooses or downloads a different CLI.
import { applyHostLaunchProfile } from '../src/main/host-launch-profile.ts';
import { homedir } from 'node:os';
import { spawnSync } from 'node:child_process';
applyHostLaunchProfile(process.env, homedir());
const result = spawnSync('/Applications/Icosa Orchestrator.app/Contents/Resources/daemon/ao', process.argv.slice(2), { env: process.env, stdio: 'inherit' });
if (result.error) { console.error(result.error.message); process.exit(1); }
process.exit(result.status ?? 1);
