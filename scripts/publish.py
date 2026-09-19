#!/usr/bin/env python3
"""Offline plan by default; --apply provisions through the operator's authenticated gh.
No token collection, unrelated-repository takeover, force push, or automatic merge.
"""
from __future__ import annotations
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
from typing import Any
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]
MARKER = '<!-- agent-council-bootstrap:v1 -->'


class ProvisionError(RuntimeError):
    pass


class APIError(ProvisionError):
    def __init__(self, message: str, status: int | None = None):
        super().__init__(message)
        self.status = status


def load(path: Path) -> Any:
    return json.loads(path.read_text(encoding='utf-8'))


def save(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    tmp = path.with_name(path.name + '.tmp')
    tmp.write_text(json.dumps(value, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')
    try:
        tmp.chmod(0o600)
    except OSError:
        pass
    tmp.replace(path)


def safe_source(root: Path, name: str) -> Path:
    p = PurePosixPath(name)
    if (not name or ':' in name or '\\' in name or p.is_absolute() or '..' in p.parts
            or any(x in {'.git', '.bootstrap', '.council', 'runs', '__pycache__'} for x in p.parts)
            or p.name.startswith('.env') or p.name.endswith(('.pem', '.key', '.p12', '.local.json'))):
        raise ProvisionError(f'Unsafe source path: {name!r}')
    target = root.joinpath(*p.parts)
    cursor = target
    while cursor != root:
        if cursor.is_symlink():
            raise ProvisionError(f'Symlink rejected: {name}')
        cursor = cursor.parent
    if not target.resolve().is_relative_to(root.resolve()) or not target.is_file():
        raise ProvisionError(f'Missing/non-file source: {name}')
    return target


def verify_manifest(root: Path) -> list[dict[str, str]]:
    manifest = load(root / 'bootstrap/files.json')
    if manifest.get('schema') != 1 or not manifest.get('files'):
        raise ProvisionError('Missing or invalid publication manifest')
    seen = set()
    for entry in manifest['files']:
        name = entry['path']
        if name in seen:
            raise ProvisionError(f'Duplicate publication path: {name}')
        seen.add(name)
        raw = safe_source(root, name).read_bytes()
        if len(raw) > 1_000_000:
            raise ProvisionError(f'Unexpectedly large source: {name}')
        raw.decode('utf-8')
        if hashlib.sha256(raw).hexdigest() != entry['sha256']:
            raise ProvisionError(f'Source changed since packaging: {name}; review before publishing')
    return manifest['files']


def ordered_issues(issues: list[dict[str, Any]]) -> list[dict[str, Any]]:
    table = {i['key']: i for i in issues}
    if len(table) != len(issues):
        raise ProvisionError('Duplicate issue keys')
    done, active, result = set(), set(), []
    def visit(key: str) -> None:
        if key not in table:
            raise ProvisionError(f'Unknown dependency: {key}')
        if key in active:
            raise ProvisionError(f'Cyclic dependency: {key}')
        if key in done:
            return
        active.add(key)
        for dep in table[key]['dependencies']:
            visit(dep)
        active.remove(key)
        done.add(key)
        result.append(table[key])
    for key in sorted(table):
        visit(key)
    return result


def verify_rules(actual: dict[str, Any], actions_id: int | None) -> None:
    if actual.get('target') != 'branch' or actual.get('enforcement') != 'active':
        raise ProvisionError('Main rules are not active branch rules')
    if 'bypass_actors' not in actual or actual['bypass_actors'] != []:
        raise ProvisionError('Bypass actors are present or unverifiable')
    if actual.get('conditions', {}).get('ref_name') != {'include': ['refs/heads/main'], 'exclude': []}:
        raise ProvisionError('Wrong protected ref')
    rules = {r['type']: r for r in actual.get('rules', [])}
    needed = {'deletion', 'non_fast_forward', 'required_linear_history', 'pull_request', 'required_status_checks'}
    if not needed.issubset(rules):
        raise ProvisionError('A main protection is missing')
    p = rules['pull_request']['parameters']
    expected = dict(required_approving_review_count=0, require_code_owner_review=False,
                    require_last_push_approval=False, required_review_thread_resolution=True,
                    dismiss_stale_reviews_on_push=True, allowed_merge_methods=['squash'])
    if any(p.get(k) != v for k, v in expected.items()):
        raise ProvisionError('PR policy differs from the agreed bootstrap policy')
    checks = rules['required_status_checks']['parameters']
    ci = next((v for v in checks.get('required_status_checks', []) if v.get('context') == 'ci'), None)
    if checks.get('strict_required_status_checks_policy') is not True or ci is None:
        raise ProvisionError('Current-base ci check is not required')
    if actions_id is not None and ci.get('integration_id') != actions_id:
        raise ProvisionError('Required CI is not bound to GitHub Actions')


def validate_seed(root: Path) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    plan = load(root / 'bootstrap/plan.json')
    if (plan['owner'], plan['name']) != ('CtrlCarlitos', 'agent-council'):
        raise ProvisionError('Publisher is scoped to the explicitly requested repository')
    issues = ordered_issues(load(root / 'bootstrap/issues.json'))
    labels = {v['name'] for v in plan['labels']}
    milestones = {v['title'] for v in plan['milestones']}
    for issue in issues:
        if not re.fullmatch(r'AC-\d{3}', issue['key']):
            raise ProvisionError('Invalid stable issue key')
        if not set(issue['labels']).issubset(labels) or issue['milestone'] not in milestones:
            raise ProvisionError('Undefined issue labels/milestone')
        if not all(issue[k] for k in ['title', 'problem', 'outcome', 'acceptance', 'risk', 'verification']):
            raise ProvisionError('Incomplete issue specification')
    verify_rules(load(root / 'bootstrap/ruleset-main.json'), None)
    return plan, issues


def issue_body(issue: dict[str, Any], links: dict[str, dict[str, Any]]) -> str:
    deps = '\n'.join(f"- {key}: #{links[key]['number']}" for key in issue['dependencies']) or 'None.'
    ac = '\n'.join(f'- [ ] {a}' for a in issue['acceptance'])
    return (f"<!-- agent-council:{issue['key']} -->\n## Problem\n\n{issue['problem']}\n\n"
            f"## Intended outcome\n\n{issue['outcome']}\n\n## Acceptance criteria\n\n{ac}\n\n"
            f"## Dependencies\n\n{deps}\n\n## Risks and boundaries\n\n{issue['risk']}\n\n"
            f"## Verification\n\n{issue['verification']}\n\n## Working agreement\n\n"
            'Read AGENTS.md and the relevant ADR. Use a task branch and PR. Do not auto-merge, '
            'change main protection, copy credentials, or call an untested adapter production-ready. '
            'Refine the scope in this issue before implementation.\n')


class GhClient:
    def __init__(self) -> None:
        if not shutil.which('gh'):
            raise ProvisionError('GitHub CLI (gh) is required. Install and authenticate it normally; never paste tokens here.')

    def call(self, method: str, endpoint: str, body: Any = None) -> Any:
        cmd = ['gh', 'api', '--hostname', 'github.com', '--method', method, endpoint,
               '-H', 'Accept: application/vnd.github+json', '-H', 'X-GitHub-Api-Version: 2022-11-28']
        data = None
        if body is not None:
            cmd += ['--input', '-']
            data = json.dumps(body, ensure_ascii=False)
        try:
            r = subprocess.run(cmd, input=data, text=True, encoding='utf-8', capture_output=True, timeout=90,
                               env=dict(os.environ, GH_PROMPT_DISABLED='1', GH_DEBUG='', GH_PAGER=''))
        except subprocess.TimeoutExpired as exc:
            raise APIError(f'{method} {endpoint}: timeout; outcome may be unknown. Reconcile before retry.') from exc
        try:
            out = json.loads(r.stdout) if r.stdout.strip() else None
        except json.JSONDecodeError as exc:
            raise APIError(f'{method} {endpoint}: non-JSON response, outcome may be unknown') from exc
        if r.returncode:
            match = re.search(r'HTTP\s+(\d{3})', r.stderr)
            status = int(match.group(1)) if match else None
            if isinstance(out, dict) and str(out.get('status', '')).isdigit():
                status = int(out['status'])
            detail = out.get('message', 'request failed') if isinstance(out, dict) else 'request failed; check gh auth status and scopes'
            raise APIError(f'{method} {endpoint}: {detail} (status {status or "unknown"})', status)
        return out


def pages(client: Any, endpoint: str) -> list[dict[str, Any]]:
    result = []
    separator = '&' if '?' in endpoint else '?'
    for page in range(1, 1001):
        batch = client.call('GET', f'{endpoint}{separator}per_page=100&page={page}')
        if not isinstance(batch, list):
            raise ProvisionError(f'Unexpected pagination result for {endpoint}')
        result.extend(batch)
        if len(batch) < 100:
            return result
    raise ProvisionError('Pagination cap reached; listing is incomplete')


class Publisher:
    def __init__(self, root: Path, client: Any, files: list[dict[str, str]], quiet: bool = False):
        self.root, self.client, self.files, self.quiet = root, client, files, quiet
        self.plan, self.issues = validate_seed(root)
        self.repo = f"{self.plan['owner']}/{self.plan['name']}"
        self.base = f'repos/{self.repo}'
        self.receipt = root / '.bootstrap/state.json'
        self.state = load(self.receipt) if self.receipt.exists() else {'repository': self.repo}
        if self.state.get('repository') != self.repo:
            raise ProvisionError('Receipt belongs to another repository')
        self.warnings: list[str] = []

    def persist(self) -> None:
        save(self.receipt, self.state)

    def log(self, message: str) -> None:
        if not self.quiet:
            print(message, flush=True)

    def optional_get(self, endpoint: str) -> Any:
        try:
            return self.client.call('GET', endpoint)
        except APIError as exc:
            if exc.status == 404:
                return None
            raise

    def ensure_repo(self) -> None:
        identity = self.client.call('GET', 'user')
        if identity.get('login', '').lower() != self.plan['owner'].lower():
            raise ProvisionError(f"Authenticate gh as {self.plan['owner']}; refusing another account")
        repo = self.optional_get(self.base)
        if repo is None:
            if self.state.get('repository_id'):
                raise ProvisionError('Recorded repository is missing/inaccessible; refusing recreation')
            self.log(f'Creating public {self.repo} with a minimal GitHub README ...')
            repo = self.client.call('POST', 'user/repos', dict(name=self.plan['name'], description=self.plan['description'],
                                    private=False, auto_init=True, has_issues=True, has_wiki=False))
            self.state['repository_id'] = repo['id']
            self.persist()
        elif self.state.get('repository_id') != repo['id']:
            raise ProvisionError('Repository exists without a matching receipt; refusing takeover')
        if repo.get('private') is not False or repo.get('full_name', '').lower() != self.repo.lower():
            raise ProvisionError('Unexpected repository identity or visibility')
        if repo.get('default_branch') != 'main':
            old = repo.get('default_branch')
            if not old:
                raise ProvisionError('Missing initialized default branch')
            self.client.call('POST', f"{self.base}/branches/{quote(old, safe='')}/rename", {'new_name': 'main'})
        self.client.call('PATCH', self.base, self.plan['settings'])
        self.client.call('PUT', f'{self.base}/actions/permissions/workflow',
                         dict(default_workflow_permissions='read', can_approve_pull_request_reviews=False))

    def ensure_rules(self) -> None:
        app = self.client.call('GET', 'apps/github-actions')
        app_id = app.get('id')
        if not isinstance(app_id, int) or app.get('slug') != 'github-actions':
            raise ProvisionError('Cannot identify the GitHub Actions integration')
        desired = load(self.root / 'bootstrap/ruleset-main.json')
        for rule in desired['rules']:
            if rule['type'] == 'required_status_checks':
                rule['parameters']['required_status_checks'][0]['integration_id'] = app_id
        matches = [v for v in pages(self.client, f'{self.base}/rulesets') if v['name'] == desired['name']]
        if len(matches) > 1:
            raise ProvisionError('Duplicate main rulesets need inspection')
        rule_id = matches[0]['id'] if matches else self.client.call('POST', f'{self.base}/rulesets', desired)['id']
        verify_rules(self.client.call('GET', f'{self.base}/rulesets/{rule_id}'), app_id)
        self.state.update(ruleset_id=rule_id, actions_app_id=app_id, protection_verified=True)
        self.persist()
        self.log('Main PR/CI rules read back successfully; no bypass actors.')

    def verify_settings(self) -> dict[str, Any]:
        repo = self.client.call('GET', self.base)
        if repo.get('id') != self.state['repository_id'] or repo.get('private') is not False:
            raise ProvisionError('Repository identity/visibility verification failed')
        for key, expected in self.plan['settings'].items():
            if repo.get(key) != expected:
                raise ProvisionError(f'Repository setting {key} did not persist')
        p = self.client.call('GET', f'{self.base}/actions/permissions/workflow')
        if p.get('default_workflow_permissions') != 'read' or p.get('can_approve_pull_request_reviews') is not False:
            raise ProvisionError('Actions default permissions not verified')
        verify_rules(self.client.call('GET', f"{self.base}/rulesets/{self.state['ruleset_id']}"), self.state['actions_app_id'])
        return repo

    def ensure_metadata(self) -> dict[str, int]:
        existing = {v['name'] for v in pages(self.client, f'{self.base}/labels')}
        for label in self.plan['labels']:
            if label['name'] not in existing:
                self.client.call('POST', f'{self.base}/labels', label)
        found = {v['title']: v for v in pages(self.client, f'{self.base}/milestones?state=all')}
        milestones = {}
        for milestone in self.plan['milestones']:
            value = found.get(milestone['title'])
            if value is None:
                value = self.client.call('POST', f'{self.base}/milestones', milestone)
            milestones[milestone['title']] = value['number']
        return milestones

    def ensure_issues(self, milestones: dict[str, int]) -> dict[str, dict[str, Any]]:
        existing = [v for v in pages(self.client, f'{self.base}/issues?state=all') if 'pull_request' not in v]
        links: dict[str, dict[str, Any]] = {}
        for issue in self.issues:
            marker = f"<!-- agent-council:{issue['key']} -->"
            matches = [v for v in existing if marker in (v.get('body') or '')]
            if len(matches) > 1:
                raise ProvisionError(f"Duplicate issue marker: {issue['key']}")
            if matches:
                value = matches[0]
                if value.get('user', {}).get('login', '').lower() != self.plan['owner'].lower():
                    raise ProvisionError('An unexpected author used a reserved issue marker')
            else:
                value = self.client.call('POST', f'{self.base}/issues', dict(
                    title=f"[{issue['key']}] {issue['title']}", body=issue_body(issue, links),
                    labels=issue['labels'], milestone=milestones[issue['milestone']]))
            links[issue['key']] = dict(number=value['number'], url=value['html_url'])
            self.state['issues'] = links
            self.persist()
        for issue in self.issues:
            actual = self.client.call('GET', f"{self.base}/issues/{links[issue['key']]['number']}")
            if actual.get('body') != issue_body(issue, links):
                raise ProvisionError(f"Issue {issue['key']} differs from the seed; preserving it for operator inspection")
            if not set(issue['labels']).issubset({v['name'] for v in actual.get('labels', [])}):
                raise ProvisionError(f"Missing labels for {issue['key']}")
            if (actual.get('milestone') or {}).get('number') != milestones[issue['milestone']]:
                raise ProvisionError(f"Milestone mismatch for {issue['key']}")
        self.log(f'Created/reconciled and verified {len(links)} elaborated issues.')
        return links

    def ensure_pr(self) -> dict[str, Any]:
        branch = self.plan['branch']
        query = quote(self.plan['owner'] + ':' + branch, safe='')
        matches = [v for v in pages(self.client, f'{self.base}/pulls?state=all&head={query}&base=main')
                   if MARKER in (v.get('body') or '')]
        if len(matches) > 1:
            raise ProvisionError('Multiple bootstrap PRs require inspection')
        if matches:
            pr = self.client.call('GET', f"{self.base}/pulls/{matches[0]['number']}")
            if pr.get('state') == 'closed' and not pr.get('merged'):
                raise ProvisionError('Bootstrap PR closed without merge; not reopening automatically')
            return pr
        base_sha = self.client.call('GET', f'{self.base}/git/ref/heads/main')['object']['sha']
        ref = self.optional_get(f'{self.base}/git/ref/heads/{branch}')
        if ref:
            if ref['object']['sha'] != self.state.get('bootstrap_commit'):
                raise ProvisionError('Unrecognized work on bootstrap branch; refusing overwrite')
            commit = ref['object']['sha']
        elif self.state.get('bootstrap_commit'):
            commit = self.state['bootstrap_commit']
            self.client.call('POST', f'{self.base}/git/refs', dict(ref='refs/heads/' + branch, sha=commit))
        else:
            base_tree = self.client.call('GET', f'{self.base}/git/commits/{base_sha}')['tree']['sha']
            entries = [dict(path=e['path'], mode='100644', type='blob',
                            content=safe_source(self.root, e['path']).read_text(encoding='utf-8')) for e in self.files]
            entries.append(dict(path='bootstrap/files.json', mode='100644', type='blob',
                                content=(self.root / 'bootstrap/files.json').read_text(encoding='utf-8')))
            tree = self.client.call('POST', f'{self.base}/git/trees', dict(base_tree=base_tree, tree=entries))['sha']
            commit = self.client.call('POST', f'{self.base}/git/commits', dict(
                message='chore: seed Agent Council foundation and development policies', tree=tree, parents=[base_sha]))['sha']
            self.state.update(bootstrap_commit=commit, bootstrap_tree=tree)
            self.persist()
            self.client.call('POST', f'{self.base}/git/refs', dict(ref='refs/heads/' + branch, sha=commit))
        body = (MARKER + '\n## Scope\n\nBootstrap the controller-led Agent Council foundation.\n\n'
                'Adds product/architecture decisions, contribution and security policies, a dependency-free Go '
                'domain kernel, CI and an elaborated issue roadmap. Native service/MCP/adapters are planned, '
                'not implemented.\n\n## Repository policy\n\nMain rules were installed/read back before this '
                'branch was created. No bypass or automatic merge. Required `ci` must pass.\n\n'
                '## Risks and limits\n\nNo native credentials, private transcripts or paid-provider tests. '
                'License awaits an explicit owner decision. This scaffold is not a working council.\n\n'
                '## Review\n\nReview README, AGENTS.md, architecture, policy and test evidence, then merge '
                'explicitly only after CI and scope are acceptable.\n')
        return self.client.call('POST', f'{self.base}/pulls', dict(
            title='chore: bootstrap Agent Council foundations', head=branch, base='main', body=body,
            maintainer_can_modify=True))

    def verify_source_tree(self, pr: dict[str, Any]) -> None:
        head = pr.get('head', {}).get('sha')
        if not head or head != self.state.get('bootstrap_commit'):
            raise ProvisionError('Bootstrap PR head changed; refusing to certify unknown source')
        tree_sha = self.client.call('GET', f'{self.base}/git/commits/{head}')['tree']['sha']
        tree = self.client.call('GET', f'{self.base}/git/trees/{tree_sha}?recursive=1')
        if tree.get('truncated'):
            raise ProvisionError('Remote source listing is truncated')
        entries = {e['path']: e for e in tree.get('tree', [])}
        for name in [e['path'] for e in self.files] + ['bootstrap/files.json']:
            raw = safe_source(self.root, name).read_bytes()
            expected = hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest()
            actual = entries.get(name, {})
            if actual.get('type') != 'blob' or actual.get('sha') != expected or actual.get('mode') != '100644':
                raise ProvisionError(f'Remote source mismatch: {name}')

    def run(self) -> dict[str, Any]:
        self.ensure_repo()
        self.ensure_rules()
        self.verify_settings()  # Fail before source publication if protection is incomplete.
        try:
            self.client.call('PUT', f'{self.base}/private-vulnerability-reporting')
            if self.client.call('GET', f'{self.base}/private-vulnerability-reporting').get('enabled') is not True:
                self.warnings.append('Private vulnerability reporting was not verified enabled.')
        except APIError:
            self.warnings.append('Private vulnerability reporting needs manual Security settings inspection.')
        milestones = self.ensure_metadata()
        links = self.ensure_issues(milestones)
        pr = self.ensure_pr()
        pr = self.client.call('GET', f"{self.base}/pulls/{pr['number']}")
        if pr.get('base', {}).get('ref') != 'main' or pr.get('head', {}).get('ref') != self.plan['branch']:
            raise ProvisionError('Bootstrap PR targets unexpected refs')
        self.verify_source_tree(pr)
        repo = self.verify_settings()
        report = dict(repository_url=repo['html_url'], public=True, main_rules_read_back=True,
                      ruleset_id=self.state['ruleset_id'], bypass_actors=[], independent_approvals=0,
                      issues=links, issue_count=len(links), milestone_count=len(milestones),
                      bootstrap_pr_url=pr['html_url'], bootstrap_pr_state=pr['state'],
                      merged=bool(pr.get('merged', False)), ci_status='Not assessed here; required before merge.',
                      warnings=self.warnings)
        save(self.root / '.bootstrap/report.json', report)
        self.state['completed'] = True
        self.persist()
        self.log(json.dumps(report, indent=2, ensure_ascii=False))
        self.log('Provisioning read back. No automatic merge; inspect bootstrap PR and CI.')
        return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='perform the described GitHub writes')
    args = parser.parse_args()
    try:
        plan, issues = validate_seed(ROOT)
        files = verify_manifest(ROOT)
        if not args.apply:
            print(f"OFFLINE PLAN: public {plan['owner']}/{plan['name']}")
            print(f"{len(files)+1} allowlisted files; {len(issues)} issues; {len(plan['milestones'])} milestones.")
            print('Minimal README -> protect main -> seed issues -> scaffold branch + PR -> read-back verification.')
            print('No GitHub requests made. Use --apply with your authenticated gh to publish.')
            return 0
        Publisher(ROOT, GhClient(), files).run()
        return 0
    except (ProvisionError, OSError, UnicodeError, json.JSONDecodeError, KeyError) as exc:
        print(f'STOPPED: {exc}', file=sys.stderr)
        print('No success claimed. A partial repository may exist if --apply had begun. Keep '
              '.bootstrap/state.json to reconcile; never force-overwrite.', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
