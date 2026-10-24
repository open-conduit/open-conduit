#!/usr/bin/env python3
"""Check B05 schema/examples and semantic negative controls; no runtime or I/O doubles.

Install scripts/requirements-config-contract.txt into a disposable venv first.
The registry below is deliberately synthetic; it does not certify actual adapters.
"""
import copy
import json
from pathlib import Path

from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parents[1]
SCHEMA = json.loads((ROOT / 'docs/architecture/config.schema.json').read_text())
VALIDATOR = Draft202012Validator(SCHEMA)
# Mechanical fixture registrations, independent of service implementations.
EXECUTORS = {
    'shell': {'toolbelt': 'cli'},
    'support_agent': {'toolbelt': 'mcp', 'inject': True},
    'coding_agent': {'toolbelt': 'mcp', 'interrupt': True, 'workspace': True},
}
HANDLERS = {'manual', 'thread', 'issue'}
EXTENSIONS = {'chat_ingress', 'code_ingress', 'schedule'}
CONNECTIONS = {'chat', 'code'}
OPERATIONS = {'emit', 'read_context', 'declare_outcome'}


def reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate_key')
        result[key] = value
    return result


def load(path):
    return json.loads(path.read_text(), object_pairs_hook=reject_duplicate_keys)


def check(config):
    """Small reference schema/semantic oracle, not a production config loader."""
    errors = []
    effective = {}

    def fail(path, code):
        errors.append((path, code))

    for error in VALIDATOR.iter_errors(config):
        fail('/'.join(map(str, error.absolute_path)), 'schema')
    if errors:
        return sorted(set(errors)), effective
    scopes, connections = {}, {}
    for i, connection in enumerate(config['connections']):
        if connection['id'] in connections:
            fail(f'connections/{i}/id', 'duplicate_id')
        connections[connection['id']] = connection
        if connection['type'] not in CONNECTIONS:
            fail(f'connections/{i}/type', 'unknown_registration')
    for i, scope in enumerate(config['scopes']):
        if scope['id'] in scopes:
            fail(f'scopes/{i}/id', 'duplicate_id')
        scopes[scope['id']] = (i, scope)
    visiting = set()

    def resolve(scope_id):
        if scope_id in effective:
            return effective[scope_id]
        if scope_id in visiting:
            fail(f'scopes/{scopes[scope_id][0]}/parent', 'scope_cycle')
            return None
        visiting.add(scope_id)
        i, scope = scopes[scope_id]
        path = f'scopes/{i}'
        parent = None
        if 'parent' in scope:
            if scope['parent'] not in scopes:
                fail(path + '/parent', 'unknown_scope')
            else:
                parent = resolve(scope['parent'])
        result = copy.deepcopy(parent) if parent else {
            'authorization': {'connections': [], 'operations': [], 'read_scopes': []},
            'limits': {k: config['settings'].get(k, d) for k, d in [('concurrency', 4), ('admission_limit', 10)]},
            'secret_refs': {}, 'bindings': {}, 'routes': {}, 'extensions': {}, 'enabled': True,
        }
        result['enabled'] = result['enabled'] and scope.get('enabled', True)
        for category, requested in scope.get('authorization', result['authorization']).items():
            if parent and not set(requested) <= set(parent['authorization'][category]):
                fail(path + '/authorization/' + category, 'authority_widening')
        result['authorization'] = copy.deepcopy(scope.get('authorization', result['authorization']))
        for category, requested in result['authorization'].items():
            available = connections if category == 'connections' else scopes if category == 'read_scopes' else OPERATIONS
            if not set(requested) <= set(available):
                fail(path + '/authorization/' + category, 'unknown_reference')
        for key, value in scope.get('limits', {}).items():
            if value > result['limits'][key]:
                fail(path + '/limits/' + key, 'limit_widening')
            result['limits'][key] = value
        result['secret_refs'].update(scope.get('secret_refs', {}))
        for category in ('bindings', 'routes', 'extensions'):
            names = set()
            for j, entry in enumerate(scope.get(category, [])):
                entry_path = f'{path}/{category}/{j}'
                name = entry['name']
                if name in names:
                    fail(entry_path + '/name', 'duplicate_name')
                names.add(name)
                if entry.get('disabled') and name not in result[category]:
                    fail(entry_path, 'unknown_disabled_entry')
                # Python dict replacement preserves the ancestor slot even for a tombstone.
                result[category][name] = copy.deepcopy(entry)
        for name, binding in result['bindings'].items():
            bp = path + '/bindings/' + name
            if binding.get('disabled'):
                continue
            capabilities = EXECUTORS.get(binding['executor'])
            if capabilities is None:
                fail(bp + '/executor', 'unknown_registration')
                continue
            for key, required in binding['requires'].items():
                if required and (capabilities.get(key, False) != required):
                    fail(bp + '/requires/' + key, 'unsupported_capability')
            if binding['requires'].get('workspace') and 'resource' not in binding:
                fail(bp + '/resource', 'resource_required')
            if 'resource' in binding and binding['resource']['provider'] != 'git':
                fail(bp + '/resource/provider', 'unknown_registration')
            if not set(binding['operations']) <= set(result['authorization']['operations']):
                fail(bp + '/operations', 'operation_denied')
            for secret in binding['secret_names']:
                if not result['secret_refs'].get(secret):
                    fail(bp + '/secret_names', 'unknown_secret')
            if 'connection' in binding and binding['connection'] not in result['authorization']['connections']:
                fail(bp + '/connection', 'connection_denied')
        for name, route in result['routes'].items():
            rp = path + '/routes/' + name
            if route.get('disabled'):
                continue
            if route['handler'] not in HANDLERS:
                fail(rp + '/handler', 'unknown_registration')
            binding = result['bindings'].get(route['binding'])
            if not binding or binding.get('disabled'):
                fail(rp + '/binding', 'missing_binding')
                continue
            policy = route['active_policy']
            capabilities = EXECUTORS.get(binding['executor'], {})
            if policy in ('inject', 'interrupt') and not capabilities.get(policy) and 'unsupported_fallback' not in route:
                fail(rp + '/active_policy', 'unsupported_policy')
        for name, extension in result['extensions'].items():
            ep = path + '/extensions/' + name
            if extension.get('disabled'):
                continue
            if extension['type'] not in EXTENSIONS:
                fail(ep + '/type', 'unknown_registration')
            if 'connection' in extension and extension['connection'] not in result['authorization']['connections']:
                fail(ep + '/connection', 'connection_denied')
            if any(not result['secret_refs'].get(s) for s in extension['secret_names']):
                fail(ep + '/secret_names', 'unknown_secret')
            if extension['type'].endswith('_ingress') and extension['config'].get('scope') != scope_id:
                fail(ep + '/config/scope', 'ingress_target_mismatch')
        visiting.remove(scope_id)
        effective[scope_id] = result
        return result

    for scope_id in scopes:
        resolve(scope_id)
    return sorted(set(errors)), effective


def main():
    Draft202012Validator.check_schema(SCHEMA)
    examples = ROOT / 'docs/architecture/examples/config'
    for name in ('workspace-free', 'git-scope', 'shell-job', 'inheritance'):
        errors, effective = check(load(examples / (name + '.json')))
        assert not errors, (name, errors)
        if name == 'inheritance':
            child = effective['child']
            assert list(child['routes']) == ['incoming', 'second']
            assert child['routes']['incoming']['active_policy'] == 'inject'
            assert child['bindings']['run']['config'] == {'command': 'printf hello'}
            assert child['secret_refs']['temporary'] is False
    invalid = load(examples / 'invalid-capability.json')
    assert any(code == 'unsupported_capability' for _, code in check(invalid)[0])
    base = load(examples / 'inheritance.json')
    command = copy.deepcopy(base)
    command['scopes'][0]['secret_refs']['temporary'] = 'cmd:printf token'
    assert not check(command)[0], 'valid cmd reference rejected'
    mutations = [
        ('schema', lambda c: c.update(unrecognized=True)),
        ('schema', lambda c: c['settings'].update(causal_max_depth=0)),
        ('schema', lambda c: c['scopes'][0]['routes'][0].update(active_policy='coalesce')),
        ('schema', lambda c: c['scopes'][0]['secret_refs'].update(temporary='literal-secret')),
        ('schema', lambda c: c['scopes'][0]['secret_refs'].update(temporary='cmd:printf token\n')),
        ('schema', lambda c: c['scopes'][0]['secret_refs'].update(temporary='cmd:printf\rtoken')),
        ('scope_cycle', lambda c: c['scopes'][0].update(parent='child')),
        ('unknown_scope', lambda c: c['scopes'][1].update(parent='absent')),
        ('duplicate_name', lambda c: c['scopes'][0]['routes'].append(copy.deepcopy(c['scopes'][0]['routes'][0]))),
        ('authority_widening', lambda c: c['scopes'][1].update(authorization={'connections': [], 'operations': ['emit', 'wait'], 'read_scopes': []})),
        ('limit_widening', lambda c: c['scopes'][1].update(limits={'concurrency': 5})),
        ('missing_binding', lambda c: c['scopes'][1]['bindings'].append({'name': 'run', 'disabled': True})),
        ('unsupported_policy', lambda c: c['scopes'][1]['routes'][0].pop('unsupported_fallback')),
        ('unknown_secret', lambda c: c['scopes'][1]['bindings'][0].update(secret_names=['temporary'])),
        ('connection_denied', lambda c: c['scopes'][1]['bindings'][0].update(connection='ungranted')),
        ('resource_required', lambda c: c['scopes'][1]['bindings'][0].update(requires={'workspace': True})),
        ('unknown_disabled_entry', lambda c: c['scopes'][1]['routes'].append({'name': 'missing', 'disabled': True})),
    ]
    for expected, mutate in mutations:
        candidate = copy.deepcopy(base)
        mutate(candidate)
        errors, _ = check(candidate)
        assert any(code == expected for _, code in errors), (expected, errors)
    # Whole replacement cannot retain undeclared operations/secrets/config from an ancestor.
    candidate = copy.deepcopy(base)
    candidate['scopes'][0]['bindings'][0]['config']['parent_only'] = True
    errors, effective = check(candidate)
    assert not errors and 'parent_only' not in effective['child']['bindings']['run']['config']
    # A masked slot may be restored by a grandchild without moving the second route.
    candidate = copy.deepcopy(base)
    candidate['scopes'][1]['routes'] = [{'name': 'incoming', 'disabled': True}]
    candidate['scopes'].append({'id': 'leaf', 'name': 'leaf', 'parent': 'child', 'routes': [copy.deepcopy(base['scopes'][0]['routes'][0])]})
    errors, effective = check(candidate)
    assert not errors and list(effective['leaf']['routes']) == ['incoming', 'second']
    try:
        json.loads('{"version":1,"version":2}', object_pairs_hook=reject_duplicate_keys)
        raise AssertionError('duplicate keys accepted')
    except ValueError:
        pass
    print('B05: schema valid; 4 valid examples, invalid capability, 17 negative mutations, inheritance/whole replacement/tombstone restoration and duplicate-key checks passed')


if __name__ == '__main__':
    main()
