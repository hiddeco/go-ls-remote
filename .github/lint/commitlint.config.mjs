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
    (msg) => /^chore\(deps\): bump /.test(msg),
    (msg) => /^ci\(github-actions\): bump /.test(msg),
    (msg) => /^ci\(deps(?:-dev)?\): bump /.test(msg),
  ],
};
