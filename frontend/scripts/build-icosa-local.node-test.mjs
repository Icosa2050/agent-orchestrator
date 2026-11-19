import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm, copyFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { buildNumbers, bundleVersion, existingNames, icosaBase, parseIcosaVersion, resolveIcosaVersion, stampVersion } from './icosa-version.mjs';

const BUILD_ROOT = 'build_root=/Volumes/aofiles/ao-builds/icosa-orchestrator';
const helper = new URL('./icosa-version.mjs', import.meta.url);

// A scratch checkout with the recipe (build root redirected) and its helper.
async function scratch(t, prefix, packageVersion = '0.14.8-icosa.2') {
 const root = await mkdtemp(path.join(tmpdir(), prefix));
 t.after(() => rm(root, { recursive: true, force: true }));
 await mkdir(path.join(root, 'frontend/scripts'), { recursive: true });
 await mkdir(path.join(root, 'frontend/out/local'), { recursive: true });
 await mkdir(path.join(root, 'published'));
 await writeFile(path.join(root, 'frontend/package.json'), JSON.stringify({ version: packageVersion }));
 await writeFile(path.join(root, 'frontend/package-lock.json'), JSON.stringify({ version: packageVersion, packages: { '': { version: packageVersion } } }));
 await copyFile(helper, path.join(root, 'frontend/scripts/icosa-version.mjs'));
 const source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
 assert.ok(source.includes(BUILD_ROOT));
 return { root, source: source.replace(BUILD_ROOT, `build_root=${path.join(root, 'published')}`), script: path.join(root, 'frontend/scripts/build-icosa-local.sh') };
}

// Any progress past the version and collision checks is a test failure, on every host.
const stopBeforePackaging = source => source.replace('[ "$(uname -m)"', 'echo "RESOLVED $version $bundle_version"; echo UNEXPECTED_PACKAGE_WORK; exit 99\n[ "$(uname -m)"');

test('version parsing', () => {
 assert.deepEqual(icosaBase('0.14.8-icosa.2'), { base: '0.14.8', floor: 2 });
 assert.deepEqual(icosaBase('0.14.8-icosa.0'), { base: '0.14.8', floor: 0 });
 assert.deepEqual(icosaBase('0.14.9'), { base: '0.14.9', floor: 0 });
 for (const bad of ['0.14', '0.14.8-beta.1', '0.14.8-icosa.01', 'v0.14.8']) assert.throws(() => icosaBase(bad));
 assert.equal(parseIcosaVersion('0.14.8-icosa.12', '0.14.8'), 12);
 for (const bad of ['0.14.8-icosa.0', '0.14.8-icosa.03', '0.14.9-icosa.1', '0.14.8-icosa.2x', '10.14.8-icosa.2']) assert.equal(parseIcosaVersion(bad, '0.14.8'), null);
});

test('next N: empty, gaps, other bases, artifacts and the floor', () => {
 const next = (packageVersion, names) => resolveIcosaVersion({ packageVersion, names }).version;
 assert.equal(next('0.14.8-icosa.0', []), '0.14.8-icosa.1');
 assert.equal(next('0.14.8-icosa.2', []), '0.14.8-icosa.3');
 assert.equal(next('0.14.8-icosa.2', ['0.14.8-icosa.1', '0.14.8-icosa.7', '0.14.8-icosa.4']), '0.14.8-icosa.8');
 assert.equal(next('0.14.8-icosa.2', ['0.14.9-icosa.30', '10.14.8-icosa.40', '0.14.80-icosa.50', 'notes']), '0.14.8-icosa.3');
 assert.equal(next('0.14.8-icosa.2', ['Icosa Orchestrator-0.14.8-icosa.9-arm64.dmg', '.build-0.14.8-icosa.11.lock']), '0.14.8-icosa.12');
 // An upstream sync that changes the base starts again at 1.
 assert.equal(next('0.14.9', ['0.14.8-icosa.20']), '0.14.9-icosa.1');
 assert.deepEqual(buildNumbers(['0.14.8-icosa.10', '0.14.8-icosa.9'], '0.14.8'), [10, 9]);
});

test('explicit version must match the package base', () => {
 const resolve = requested => resolveIcosaVersion({ packageVersion: '0.14.8-icosa.2', names: ['0.14.8-icosa.9'], requested });
 assert.deepEqual(resolve('0.14.8-icosa.20'), { version: '0.14.8-icosa.20', base: '0.14.8', bundleVersion: '0.14.8020' });
 for (const bad of ['0.14.9-icosa.1', '0.14.8-icosa.0', '0.14.8', '0.14.8-icosa.x']) assert.throws(() => resolve(bad), /must match/);
});

test('CFBundleVersion is numeric and ordered across N and bases', () => {
 assert.equal(bundleVersion('0.14.8-icosa.3'), '0.14.8003');
 const order = ['0.14.8-icosa.9', '0.14.8-icosa.10', '0.14.9-icosa.1', '0.15.0-icosa.1', '1.0.0-icosa.1'].map(bundleVersion);
 const key = v => v.split('.').map(Number);
 for (let i = 1; i < order.length; i++) {
  const [a, b] = [key(order[i - 1]), key(order[i])];
  assert.ok(a[0] < b[0] || (a[0] === b[0] && (a[1] < b[1] || (a[1] === b[1] && a[2] < b[2]))), `${order[i - 1]} < ${order[i]}`);
 }
 assert.throws(() => bundleVersion('0.14.8-icosa.1000'), /exceeds/);
});

test('auto-numbering refuses an unlistable build root, explicit versions do not', async t => {
 const root = await mkdtemp(path.join(tmpdir(), 'icosa-names-'));
 t.after(() => rm(root, { recursive: true, force: true }));
 await writeFile(path.join(root, 'manifest.json'), '{"version":"0.14.8-icosa.6"}');
 assert.throws(() => existingNames({ buildRoot: path.join(root, 'missing'), local: root, requireBuildRoot: true }), /set ICOSA_VERSION/);
 assert.ok(existingNames({ buildRoot: path.join(root, 'missing'), local: root, requireBuildRoot: false }).includes('0.14.8-icosa.6'));
});

test('stamping rewrites package.json and the lockfile root', async t => {
 const root = await mkdtemp(path.join(tmpdir(), 'icosa-stamp-'));
 t.after(() => rm(root, { recursive: true, force: true }));
 const lock = path.join(root, 'package-lock.json');
 await writeFile(lock, JSON.stringify({ name: 'x', version: '0.14.8-icosa.2', packages: { '': { version: '0.14.8-icosa.2' }, 'node_modules/a': { version: '1.0.0' } } }));
 stampVersion(lock, '0.14.8-icosa.5');
 const stamped = JSON.parse(await readFile(lock, 'utf8'));
 assert.equal(stamped.version, '0.14.8-icosa.5');
 assert.equal(stamped.packages[''].version, '0.14.8-icosa.5');
 assert.equal(stamped.packages['node_modules/a'].version, '1.0.0');
});

test('recipe stamps the next free version from the build root and out/local', async t => {
 const { root, source, script } = await scratch(t, 'icosa-next-');
 await mkdir(path.join(root, 'published/0.14.8-icosa.4'));
 await mkdir(path.join(root, 'published/0.14.9-icosa.30'));
 await writeFile(path.join(root, 'frontend/out/local/Icosa Orchestrator-0.14.8-icosa.5-arm64.zip'), 'older local build');
 await writeFile(script, stopBeforePackaging(source));
 const result = spawnSync('bash', [script], { encoding: 'utf8', env: { ...process.env, ICOSA_VERSION: '' } });
 assert.equal(result.status, 99, result.stderr);
 assert.match(result.stdout, /RESOLVED 0\.14\.8-icosa\.6 0\.14\.8006/);
});

test('recipe refuses an explicit version for another base', async t => {
 const { script, source } = await scratch(t, 'icosa-base-');
 await writeFile(script, stopBeforePackaging(source));
 const result = spawnSync('bash', [script], { encoding: 'utf8', env: { ...process.env, ICOSA_VERSION: '0.14.9-icosa.1' } });
 assert.equal(result.status, 1, result.stderr);
 assert.match(result.stderr, /must match/);
 assert.doesNotMatch(result.stdout, /UNEXPECTED_PACKAGE_WORK/);
});

for (const collision of ['destination', 'zip', 'dmg', 'manifest', 'malformed manifest']) {
 test(`refuses ${collision} before any package or notary work`, async t => {
  const { root, source, script } = await scratch(t, 'icosa-build-');
  await writeFile(script, stopBeforePackaging(source));
  if (collision === 'destination') await mkdir(path.join(root, 'published/0.14.8-icosa.2'));
  else if (collision.includes('manifest')) await writeFile(path.join(root, 'frontend/out/local/manifest.json'), collision === 'manifest' ? '{"version":"0.14.8-icosa.2"}' : 'invalid');
  else await writeFile(path.join(root, `frontend/out/local/Icosa Orchestrator-0.14.8-icosa.2-arm64.${collision}`), 'pilot');
  const result = spawnSync('bash', [script], { encoding: 'utf8', env: { ...process.env, ICOSA_VERSION: '0.14.8-icosa.2' } });
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /already built or reserved.*Never overwrite artifacts/);
  assert.doesNotMatch(result.stdout, /UNEXPECTED_PACKAGE_WORK/);
 });
}

test('unavailable notary profile still fails before npm, with the version stamped', async t => {
 const { root, source, script } = await scratch(t, 'icosa-notary-');
 await mkdir(path.join(root, 'bin'));
 await writeFile(script, source);
 for (const [name, body] of Object.entries({uname:'echo arm64',git:'exit 0',xcrun:'exit 1',nice:'echo PACKAGING_REACHED; exit 99'})) {
  await writeFile(path.join(root, 'bin', name), `#!/bin/sh\n${body}\n`, {mode:0o755});
 }
 const result = spawnSync('/bin/bash', [script], {encoding:'utf8',env:{...process.env,ICOSA_VERSION:'',PATH:`${root}/bin:${process.env.PATH}`}});
 assert.equal(result.status, 1, result.stderr);
 assert.match(result.stderr, /Notarization is required/);
 assert.doesNotMatch(result.stdout, /PACKAGING_REACHED/);
 // git is stubbed here, so the exit trap's restore is a no-op and the stamp stays visible.
 assert.equal(JSON.parse(await readFile(path.join(root, 'frontend/package.json'), 'utf8')).version, '0.14.8-icosa.3');
 assert.equal(JSON.parse(await readFile(path.join(root, 'frontend/package-lock.json'), 'utf8')).packages[''].version, '0.14.8-icosa.3');
});

for (const scenario of ['existing destination', 'uncreatable parent', 'copy succeeds']) {
 test(`publication tail handles ${scenario}`, async t => {
  const root = await mkdtemp(path.join(tmpdir(), 'icosa-publish-'));
  t.after(() => rm(root, {recursive:true, force:true}));
  await mkdir(path.join(root, 'out/local'), {recursive:true});
  for (const file of ['archive.zip', 'archive.dmg', 'out/local/manifest.json', 'out/local/verification.log', 'out/local/verify-mac-artifact.sh']) {
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
   assert.match(result.stderr, /Never overwrite artifacts/);
   assert.equal(await readFile(path.join(destination, 'archive.zip'), 'utf8'), 'pilot');
  } else {
   assert.equal(result.status, 0, result.stderr);
   assert.match(result.stdout, /Artifacts remain in/);
   assert.doesNotMatch(result.stderr, /Never overwrite artifacts/);
   if (scenario === 'uncreatable parent') assert.match(result.stderr, /Manual Terminal-context copy required/);
   else {
    assert.match(result.stdout, /Copied to/);
    assert.equal(await readFile(path.join(destination, 'archive.zip'), 'utf8'), 'verified build');
    assert.equal(await readFile(path.join(destination, 'verify-mac-artifact.sh'), 'utf8'), 'verified build');
   }
  }
 });
}

const DR = 'designated => identifier "com.icosa2050.orchestrator" and anchor apple generic and certificate leaf[subject.OU] = L98MZWWR98';
for (const previous of ['none', 'same', 'changed']) {
 test(`manifest records version, shipped verifier and DR check (previous build: ${previous})`, async t => {
  const root = await mkdtemp(path.join(tmpdir(), 'icosa-manifest-'));
  t.after(() => rm(root, {recursive:true, force:true}));
  const app = path.join(root, 'Icosa Orchestrator.app');
  await mkdir(path.join(app, 'Contents/Resources/daemon'), {recursive:true});
  await mkdir(path.join(root, 'bin'));
  await mkdir(path.join(root, 'out/local'), {recursive:true});
  await mkdir(path.join(root, 'published/0.14.8-icosa.2'), {recursive:true});
  const plist = spawnSync('python3', ['-c', `import plistlib,sys; plistlib.dump(dict(CFBundleIdentifier='com.icosa2050.orchestrator',CFBundleExecutable='icosa-orchestrator',CFBundleName='Icosa Orchestrator',CFBundleDisplayName='Icosa Orchestrator',CFBundleURLTypes=[dict(CFBundleURLSchemes=['icosa-orchestrator'])],CFBundleShortVersionString='0.14.8-icosa.3',CFBundleVersion='0.14.8003'),open(sys.argv[1],'wb'))`, path.join(app, 'Contents/Info.plist')]);
  assert.equal(plist.status, 0, plist.stderr?.toString());
  await writeFile(path.join(app, 'Contents/Resources/daemon/ao'), '#!/bin/sh\necho "0.14.8-icosa.3 abc"\n', {mode:0o755});
  await writeFile(path.join(root, 'bin/codesign'), `#!/bin/sh\necho "Executable=$4/Contents/MacOS/icosa-orchestrator" >&2\necho '${DR}'\n`, {mode:0o755});
  if (previous !== 'none') {
   const old = previous === 'same' ? DR : DR.replace('L98MZWWR98', 'OTHERTEAM1');
   await writeFile(path.join(root, 'published/0.14.8-icosa.2/manifest.json'), JSON.stringify({designatedRequirement: `Executable=/elsewhere\n${old}\n`}));
  }
  for (const file of ['a.zip', 'a.dmg', 'out/local/verify-mac-artifact.sh']) await writeFile(path.join(root, file), file);
  const source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
  const python = source.slice(source.indexOf("<<'PY' >> out/local/verification.log\n") + 37, source.indexOf('\nPY\n# Reserve'));
  const result = spawnSync('python3', ['-', app, path.join(root, 'a.zip'), path.join(root, 'a.dmg'), 'c0ffee', '0.14.8-icosa.3', '0.14.8003', path.join(root, 'published')], {input: python, cwd: root, encoding: 'utf8', env: {...process.env, PATH: `${root}/bin:${process.env.PATH}`, AO_NOTARY_PROFILE: 'p'}});
  assert.match(result.stdout, /designated requirement version-independent: ok/);
  if (previous === 'changed') {
   assert.notEqual(result.status, 0);
   assert.match(result.stdout, /matches 0\.14\.8-icosa\.2: CHANGED/);
   assert.match(result.stderr, /TCC grants would be lost/);
   return;
  }
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, previous === 'same' ? /matches 0\.14\.8-icosa\.2: ok/ : /previous build: none/);
  const manifest = JSON.parse(await readFile(path.join(root, 'out/local/manifest.json'), 'utf8'));
  assert.equal(manifest.version, '0.14.8-icosa.3');
  assert.equal(manifest.bundleVersion, '0.14.8003');
  assert.equal(manifest.verifier, 'verify-mac-artifact.sh');
  assert.equal(manifest.verifierSha256, createHash('sha256').update('out/local/verify-mac-artifact.sh').digest('hex'));
  assert.equal(manifest.artifacts.length, 2);
 });
}

test('recipe ships the verifier and restores package files', async () => {
 const source = await readFile(new URL('./build-icosa-local.sh', import.meta.url), 'utf8');
 assert.match(source, /cp -p scripts\/verify-mac-artifact\.sh out\/local\/verify-mac-artifact\.sh/);
 assert.match(source, /out\/local\/verify-mac-artifact\.sh "\$destination\/"/);
 assert.match(source, /export ICOSA_BUNDLE_VERSION="\$bundle_version"/);
 assert.match(source, /trap 'git -C "\$repo" checkout -q -- frontend\/package\.json frontend\/package-lock\.json/);
});
