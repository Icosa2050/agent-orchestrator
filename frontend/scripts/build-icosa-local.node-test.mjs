import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

for (const collision of ['destination', 'zip', 'dmg', 'manifest', 'malformed manifest']) {
 test(`refuses ${collision} before any package or notary work`, async t => {
  const root = await mkdtemp(path.join(tmpdir(), 'icosa-build-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(path.join(root, 'frontend/scripts'), { recursive: true });
  await mkdir(path.join(root, 'frontend/out/local'), { recursive: true });
  await writeFile(path.join(root, 'frontend/package.json'), '{"version":"0.14.8-icosa.2"}');
  const destination = path.join(root, 'published');
  let source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
  source = source.replace('/Volumes/aofiles/ao-builds/icosa-orchestrator/$version', destination);
  // Any progress past the collision checks is a test failure, on every host.
  source = source.replace('[ "$(uname -m)"', 'echo UNEXPECTED_PACKAGE_WORK; exit 99\n[ "$(uname -m)"');
  const script = path.join(root, 'frontend/scripts/build-icosa-local.sh');
  await writeFile(script, source);
  if (collision === 'destination') await mkdir(destination);
  else if (collision.includes('manifest')) await writeFile(path.join(root, 'frontend/out/local/manifest.json'), collision === 'manifest' ? '{"version":"0.14.8-icosa.2"}' : 'invalid');
  else await writeFile(path.join(root, `frontend/out/local/Icosa Orchestrator-0.14.8-icosa.2-arm64.${collision}`), 'pilot');
  const result = spawnSync('bash', [script], { encoding: 'utf8' });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /Bump the version/);
  assert.doesNotMatch(result.stdout, /UNEXPECTED_PACKAGE_WORK/);
 });
}

test('unavailable notary profile still fails before npm', async t => {
 const root = await mkdtemp(path.join(tmpdir(), 'icosa-notary-'));
 t.after(() => rm(root, { recursive: true, force: true }));
 await mkdir(path.join(root, 'frontend/scripts'), {recursive:true});
 await mkdir(path.join(root, 'bin'));
 await writeFile(path.join(root, 'frontend/package.json'), '{"version":"0.14.8-icosa.2"}');
 const script = path.join(root, 'frontend/scripts/build-icosa-local.sh');
 let source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
 source = source.replace('/Volumes/aofiles/ao-builds/icosa-orchestrator/$version', path.join(root, 'published'));
 await writeFile(script, source);
 for (const [name, body] of Object.entries({uname:'echo arm64',git:'exit 0',xcrun:'exit 1',nice:'echo PACKAGING_REACHED; exit 99'})) {
  await writeFile(path.join(root, 'bin', name), `#!/bin/sh\n${body}\n`, {mode:0o755});
 }
 const result = spawnSync('/bin/bash', [script], {encoding:'utf8',env:{...process.env,PATH:`${root}/bin:${process.env.PATH}`}});
 assert.equal(result.status, 1, result.stderr);
 assert.match(result.stderr, /Notarization is required/);
 assert.doesNotMatch(result.stdout, /PACKAGING_REACHED/);
});
