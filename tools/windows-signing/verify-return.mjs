import { appendFileSync, lstatSync, readFileSync } from 'node:fs';
import path from 'node:path';
import {
  executableHash,
  readBuildRun,
  readSigningRun,
  sourceFor,
  validateManifest,
} from './provenance.mjs';

const [key, signingId, directory] = process.argv.slice(2);
if (
  process.env.GITHUB_REF !== 'refs/heads/main' ||
  process.env.GITHUB_REPOSITORY !== sourceFor(key).repo
) {
  throw new Error('Runtime publication must be dispatched from its own main branch.');
}
await readSigningRun(signingId);
const manifestPath = path.join(directory, 'signed-runtime-manifest.json');
const stat = lstatSync(manifestPath);
if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 64 * 1024)
  throw new Error('Invalid signing manifest file.');
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
const build = await readBuildRun(key, manifest.sourceRunId);
validateManifest(manifest, build, signingId, process.env.RUNTIME_SIGNER_THUMBPRINT);
for (const file of manifest.files) {
  if (executableHash(directory, file.name) !== file.signedSha256)
    throw new Error('Signed executable hash mismatch.');
}
appendFileSync(
  process.env.GITHUB_OUTPUT,
  `tag=${build.sourceTag}\nsha=${build.sourceSha}\nrun-id=${build.sourceRunId}\n`,
);
