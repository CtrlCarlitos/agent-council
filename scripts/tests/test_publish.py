"""Mock/API-contract tests only. No network, native account, or actual GitHub mutation."""
from __future__ import annotations
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from urllib.parse import urlsplit

SOURCE = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('publish', SOURCE / 'scripts/publish.py')
publish = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publish)


class FakeGitHub:
    def __init__(self):
        self.login = 'CtrlCarlitos'
        self.base = 'repos/CtrlCarlitos/agent-council'
        self.repo = None
        self.settings = {}
        self.rules = {}
        self.labels = []
        self.milestones = []
        self.issues = []
        self.prs = []
        self.refs = {'main': 'base-sha'}
        self.commits = {'base-sha': {'tree': {'sha': 'base-tree'}}}
        self.trees = {'base-tree': {'tree': [], 'truncated': False}}
        self.calls = []
        self.bad_rules = False
        self.optional_security_fails = False

    def call(self, method, endpoint, body=None):
        self.calls.append((method, endpoint, copy.deepcopy(body)))
        return copy.deepcopy(self.handle(method, endpoint, body))

    def handle(self, method, endpoint, body):
        path = urlsplit(endpoint).path
        if path == 'user':
            return {'login': self.login}
        if path == 'apps/github-actions':
            return {'id': 321, 'slug': 'github-actions'}
        if path == 'user/repos' and method == 'POST':
            if self.repo:
                raise publish.APIError('name already exists', 422)
            self.repo = dict(id=123, full_name='CtrlCarlitos/agent-council', private=False,
                             html_url='https://github.com/CtrlCarlitos/agent-council', default_branch='main')
            return self.repo
        if path == self.base:
            if self.repo is None:
                raise publish.APIError('not found', 404)
            if method == 'PATCH':
                self.repo.update(body)
            return self.repo
        tail = path.removeprefix(self.base + '/')
        if tail == 'actions/permissions/workflow':
            if method == 'PUT':
                self.settings = copy.deepcopy(body)
            return self.settings
        if tail == 'rulesets':
            if method == 'POST':
                self.rules[45] = dict(copy.deepcopy(body), id=45)
                return self.rules[45]
            return list(self.rules.values())
        if tail.startswith('rulesets/'):
            value = copy.deepcopy(self.rules[int(tail.split('/')[-1])])
            if self.bad_rules:
                value['bypass_actors'] = [{'actor_type': 'RepositoryRole', 'actor_id': 5}]
            return value
        if tail == 'private-vulnerability-reporting':
            if self.optional_security_fails:
                raise publish.APIError('unsupported', 403)
            return {'enabled': True}
        if tail == 'labels':
            if method == 'POST':
                self.labels.append(copy.deepcopy(body))
                return self.labels[-1]
            return self.labels
        if tail == 'milestones':
            if method == 'POST':
                self.milestones.append(dict(copy.deepcopy(body), number=len(self.milestones)+1))
                return self.milestones[-1]
            return self.milestones
        if tail == 'issues':
            if method == 'POST':
                v = copy.deepcopy(body)
                v.update(number=len(self.issues)+1, user={'login': self.login})
                v['html_url'] = self.repo['html_url'] + '/issues/' + str(v['number'])
                v['labels'] = [{'name': s} for s in v['labels']]
                v['milestone'] = {'number': v['milestone']}
                self.issues.append(v)
                return v
            return self.issues
        if tail.startswith('issues/'):
            return self.issues[int(tail.split('/')[-1])-1]
        if tail.startswith('git/ref/heads/'):
            name = tail.removeprefix('git/ref/heads/')
            if name not in self.refs:
                raise publish.APIError('not found', 404)
            return {'object': {'sha': self.refs[name]}}
        if tail == 'git/refs' and method == 'POST':
            name = body['ref'].removeprefix('refs/heads/')
            if name in self.refs:
                raise publish.APIError('ref exists', 422)
            self.refs[name] = body['sha']
            return {'ref': body['ref'], 'object': {'sha': body['sha']}}
        if tail.startswith('git/commits/'):
            return self.commits[tail.removeprefix('git/commits/')]
        if tail == 'git/commits' and method == 'POST':
            sha = 'new-commit'
            self.commits[sha] = {'sha': sha, 'tree': {'sha': body['tree']}}
            return self.commits[sha]
        if tail == 'git/trees' and method == 'POST':
            rows = []
            for entry in body['tree']:
                raw = entry['content'].encode()
                sha = hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest()
                rows.append({k: entry[k] for k in ('path', 'mode', 'type')} | {'sha': sha})
            self.trees['new-tree'] = {'tree': rows, 'truncated': False}
            return {'sha': 'new-tree'}
        if tail.startswith('git/trees/'):
            return self.trees[tail.removeprefix('git/trees/')]
        if tail == 'pulls':
            if method == 'POST':
                pr = dict(number=23, state='open', merged=False, body=body['body'],
                          html_url=self.repo['html_url'] + '/pull/23',
                          base={'ref': body['base']}, head={'ref': body['head'], 'sha': self.refs[body['head']]})
                self.prs.append(pr)
                return pr
            return self.prs
        if tail.startswith('pulls/'):
            return self.prs[0]
        raise AssertionError((method, endpoint, body))


class PublisherTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        (self.root/'bootstrap').mkdir()
        for name in ['plan.json', 'ruleset-main.json', 'issues.json']:
            (self.root/'bootstrap'/name).write_bytes((SOURCE/'bootstrap'/name).read_bytes())
        (self.root/'README.md').write_text('# safe test source\n')
        self.files = [{'path': 'README.md', 'sha256': hashlib.sha256((self.root/'README.md').read_bytes()).hexdigest()}]
        publish.save(self.root/'bootstrap/files.json', {'schema': 1, 'files': self.files})
        self.fake = FakeGitHub()

    def tearDown(self):
        self.temp.cleanup()

    def runner(self):
        return publish.Publisher(self.root, self.fake, self.files, quiet=True)

    def test_complete_bootstrap_is_pr_only_and_verified(self):
        report = self.runner().run()
        self.assertTrue(report['main_rules_read_back'])
        self.assertEqual(report['issue_count'], 22)
        self.assertEqual(report['milestone_count'], 5)
        self.assertFalse(report['merged'])
        self.assertEqual(self.fake.refs['main'], 'base-sha')
        methods = [(m, e.split('?')[0]) for m, e, _ in self.fake.calls]
        rule_index = methods.index(('POST', self.fake.base+'/rulesets'))
        commit_index = methods.index(('POST', self.fake.base+'/git/commits'))
        self.assertLess(rule_index, commit_index)
        self.assertFalse(any('/merge' in endpoint for _, endpoint, _ in self.fake.calls))
        self.assertTrue(all('## Acceptance criteria' in i['body'] for i in self.fake.issues))
        dependent = next(i for i in self.fake.issues if '[AC-016]' in i['title'])
        self.assertIn('AC-007: #', dependent['body'])

    def test_second_run_does_not_duplicate_remote_objects(self):
        self.runner().run()
        counts = (len(self.fake.issues), len(self.fake.rules), len(self.fake.prs), len(self.fake.milestones))
        self.runner().run()
        self.assertEqual(counts, (len(self.fake.issues), len(self.fake.rules), len(self.fake.prs), len(self.fake.milestones)))

    def test_existing_repo_without_receipt_is_not_adopted(self):
        self.fake.handle('POST', 'user/repos', {})
        with self.assertRaisesRegex(publish.ProvisionError, 'takeover'):
            self.runner().run()
        self.assertFalse(any(method != 'GET' for method, _, _ in self.fake.calls))

    def test_wrong_account_is_rejected_before_writes(self):
        self.fake.login = 'someone-else'
        with self.assertRaises(publish.ProvisionError):
            self.runner().run()
        self.assertIsNone(self.fake.repo)

    def test_bad_rules_fail_before_issues_and_source(self):
        self.fake.bad_rules = True
        with self.assertRaisesRegex(publish.ProvisionError, 'Bypass'):
            self.runner().run()
        self.assertIsNotNone(self.fake.repo)  # Partial repository must not be called secured.
        self.assertFalse(self.fake.issues)
        self.assertEqual(self.fake.refs, {'main': 'base-sha'})

    def test_changed_issue_is_preserved_and_not_overwritten(self):
        self.runner().run()
        self.fake.issues[0]['body'] += '\nOperator edits.\n'
        with self.assertRaisesRegex(publish.ProvisionError, 'preserving'):
            self.runner().run()
        self.assertIn('Operator edits.', self.fake.issues[0]['body'])

    def test_missing_issue_labels_are_not_reported_successful(self):
        self.runner().run()
        self.fake.issues[0]['labels'] = []
        with self.assertRaisesRegex(publish.ProvisionError, 'Missing labels'):
            self.runner().run()

    def test_unrecognized_issue_author_is_rejected(self):
        self.runner().run()
        self.fake.issues[0]['user']['login'] = 'unexpected'
        with self.assertRaisesRegex(publish.ProvisionError, 'unexpected author'):
            self.runner().run()

    def test_remote_source_tampering_is_detected(self):
        self.runner().run()
        self.fake.trees['new-tree']['tree'][0]['sha'] = 'bad'
        with self.assertRaisesRegex(publish.ProvisionError, 'source mismatch'):
            self.runner().run()

    def test_optional_security_failure_is_a_visible_warning(self):
        self.fake.optional_security_fails = True
        report = self.runner().run()
        self.assertTrue(report['warnings'])
        self.assertTrue(report['main_rules_read_back'])

    def test_closed_unmerged_pr_is_not_reopened(self):
        self.runner().run()
        self.fake.prs[0]['state'] = 'closed'
        with self.assertRaisesRegex(publish.ProvisionError, 'closed without merge'):
            self.runner().run()

    def test_missing_repository_with_receipt_is_not_recreated(self):
        self.runner().run()
        self.fake.repo = None
        with self.assertRaisesRegex(publish.ProvisionError, 'refusing recreation'):
            self.runner().run()

    def test_payload_paths_hashes_and_symlinks(self):
        self.assertEqual(publish.verify_manifest(self.root), self.files)
        for path in ['../outside', '/absolute', '.env', 'C:/outside', '.git/config', '.bootstrap/state.json', 'secret.key', 'x.local.json']:
            with self.subTest(path=path), self.assertRaises(publish.ProvisionError):
                publish.safe_source(self.root, path)
        (self.root/'README.md').write_text('modified')
        with self.assertRaisesRegex(publish.ProvisionError, 'changed since packaging'):
            publish.verify_manifest(self.root)

    def test_symlink_source_is_not_published(self):
        (self.root/'link.md').symlink_to(self.root/'README.md')
        with self.assertRaisesRegex(publish.ProvisionError, 'Symlink'):
            publish.safe_source(self.root, 'link.md')

    def test_dependency_cycles_unknowns_and_duplicate_keys(self):
        for issues in [[{'key':'a','dependencies':['b']},{'key':'b','dependencies':['a']}],
                       [{'key':'a','dependencies':['missing']}],
                       [{'key':'a','dependencies':[]},{'key':'a','dependencies':[]}]]:
            with self.assertRaises(publish.ProvisionError):
                publish.ordered_issues(issues)

    def test_rules_require_no_bypass_and_correct_check_source(self):
        rules = publish.load(self.root/'bootstrap/ruleset-main.json')
        with self.assertRaisesRegex(publish.ProvisionError, 'bound'):
            publish.verify_rules(rules, 321)
        rules['bypass_actors'] = [{'actor_type':'RepositoryRole','actor_id':5}]
        with self.assertRaisesRegex(publish.ProvisionError, 'Bypass'):
            publish.verify_rules(rules, None)


if __name__ == '__main__':
    unittest.main()
