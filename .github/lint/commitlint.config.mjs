export default {
  extends: ['@commitlint/config-conventional'],
  rules: {
    'type-enum': [2, 'always', ['feat', 'fix', 'chore', 'refactor', 'test', 'perf', 'ci']],
    'header-max-length': [2, 'always', 52],
    'body-max-line-length': [2, 'always', 72],
    'subject-case': [0],
    'signed-off-by': [0],
  },
  ignores: [
    // Dependabot autogen subjects: matches both individual
    // (`bump foo from x to y`) and grouped (`Bump the X group
    // in <dir> with N updates`) update shapes, lower- or
    // capitalised. Covers all three ecosystems (`chore(deps)`,
    // `ci(github-actions)`, `ci(deps)`, `ci(deps-dev)`).
    (msg) => /^(chore|ci)\([^)]+\): [Bb]ump /.test(msg),
  ],
};
