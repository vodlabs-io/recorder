// devBuildInfo describes the git checkout a development build runs from, e.g.
// "main @ 1a2b3c4, modified". A dev build runs from build/ inside the checkout,
// so git finds it from this file's folder. Null when there is no checkout.
export function devBuildInfo(): string | null {
  const git = (...args: string[]): string | null => {
    try {
      const res = Bun.spawnSync(["git", ...args], { cwd: import.meta.dir });
      return res.exitCode === 0 ? res.stdout.toString().trim() : null;
    } catch {
      return null;
    }
  };
  const commit = git("rev-parse", "--short", "HEAD");
  if (!commit) return null;
  const branch = git("rev-parse", "--abbrev-ref", "HEAD");
  const modified = !!git("status", "--porcelain", "--untracked-files=no");
  const ref = branch && branch !== "HEAD" ? `${branch} @ ${commit}` : commit;
  return modified ? `${ref}, modified` : ref;
}

// devBuildMessage is the Update tab line for a development build.
export function devBuildMessage(from: string | null): string {
  return from
    ? `Development build from ${from}; updates are off.`
    : "Development build; updates are off.";
}
