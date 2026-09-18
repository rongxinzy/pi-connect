import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { sourceFor } from './provenance.mjs';

const [directory, tag, revision] = process.argv.slice(2);
const source = sourceFor('sidecar');
if (!directory || !source.tag.test(tag ?? '') || !/^[0-9a-f]{40}$/.test(revision ?? '')) {
  throw new Error('Release directory, immutable source tag, and source revision are required.');
}
const filenames = [
  'cc-connect-sidecar-windows-amd64.exe',
  'cc-connect-sidecar-darwin-amd64',
  'cc-connect-sidecar-darwin-arm64',
  'cc-connect-sidecar-linux-amd64',
];
const sha256 = Object.fromEntries(filenames.map(filename => {
  const file = path.join(directory, filename);
  const stat = lstatSync(file);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.size < 2 || stat.size > 256 * 1024 * 1024) {
    throw new Error('Runtime binary must be a bounded regular file.');
  }
  return [filename, createHash('sha256').update(readFileSync(file)).digest('hex')];
}));
writeFileSync(path.join(directory, 'checksums.txt'),
  filenames.map(filename => `${sha256[filename]}  ${filename}\n`).join(''));
writeFileSync(path.join(directory, 'runtime-manifest.json'), JSON.stringify({
  schemaVersion: 1,
  sourceRepository: `https://github.com/${source.repo}`,
  tag,
  sourceRevision: revision,
  sha256,
}, null, 2) + '\n');
