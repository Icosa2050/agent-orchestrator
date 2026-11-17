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

for (const scenario of ['existing destination', 'uncreatable parent', 'copy succeeds']) {
 test(`publication tail handles ${scenario}`, async t => {
  const root = await mkdtemp(path.join(tmpdir(), 'icosa-publish-'));
  t.after(() => rm(root, {recursive:true, force:true}));
  await mkdir(path.join(root, 'out/local'), {recursive:true});
  for (const file of ['archive.zip', 'archive.dmg', 'out/local/manifest.json', 'out/local/verification.log']) {
   await writeFile(path.join(root, file), 'verified build');
  }
  const destination = path.join(root, scenario === 'uncreatable parent' ? 'blocked/version' : 'published/version');
  if (scenario === 'existing destination') {
   await mkdir(destination, {recursive:true});
   await writeFile(path.join(destination, 'archive.zip'), 'pilot');
  } else if (scenario === 'uncreatable parent') {
   // A file as parent fails reliably, even under root (unlike chmod-based tests).
   await writeFile(path.join(root, 'blocked'), 'not a directory');
  }
  const source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
  const refuse = source.match(/^refuse\(\).*$/m)[0];
  const tail = source.slice(source.indexOf('# Reserve atomically before copying:'));
  const script = `set -euo pipefail\ncd "$BUILD_TEST_ROOT"\nversion=0.14.8-icosa.2\nzip="$PWD/archive.zip"\ndmg="$PWD/archive.dmg"\n${refuse}\n${tail}`;
  const result = spawnSync('bash', ['-c', script], {encoding:'utf8', env:{...process.env, BUILD_TEST_ROOT:root, destination}});
  if (scenario === 'existing destination') {
   assert.equal(result.status, 1, result.stderr);
   assert.match(result.stderr, /Bump the version/);
   assert.equal(await readFile(path.join(destination, 'archive.zip'), 'utf8'), 'pilot');
  } else {
   assert.equal(result.status, 0, result.stderr);
   assert.match(result.stdout, /Artifacts remain in/);
   assert.doesNotMatch(result.stderr, /Bump the version/);
   if (scenario === 'uncreatable parent') assert.match(result.stderr, /Manual Terminal-context copy required/);
   else {
    assert.match(result.stdout, /Copied to/);
    assert.equal(await readFile(path.join(destination, 'archive.zip'), 'utf8'), 'verified build');
   }
  }
 });
}
