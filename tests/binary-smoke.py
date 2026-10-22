"""Linux release smoke test: isolated data, real controller and local agent."""
import http.cookiejar, json, os, pathlib, re, secrets, signal, socket, ssl, subprocess, sys, tempfile, time, urllib.request
binary = str(pathlib.Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix='lanforge-smoke-') as data:
    password = secrets.token_hex(16)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
    env = dict(os.environ, LANFORGE_PASSWORD=password)
    subprocess.run([binary, 'init', '--data', data, '--port', str(port)], env=env, check=True)
    proc = subprocess.Popen([binary, 'run', '--data', data, '--no-browser'], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    # The test alone trusts its own freshly generated local certificate.
    opener = urllib.request.build_opener(urllib.request.HTTPSHandler(context=ssl._create_unverified_context()), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    base = f'https://localhost:{port}'
    def get(path):
        with opener.open(base+path, timeout=5) as r: return r.read()
    try:
        for _ in range(40):
            try: html = get('/').decode(); break
            except Exception: time.sleep(.25)
        else: raise RuntimeError('controller did not start')
        assert 'id="root"' in html
        for asset in re.findall(r'(?:src|href)="(/assets/[^"]+)"', html):
            assert len(get(asset)) > 100
        request = urllib.request.Request(base+'/api/login', data=json.dumps({'password':password}).encode(), headers={'Content-Type':'application/json','X-LanForge':'1','Origin':base})
        assert json.load(opener.open(request, timeout=5))['ok']
        catalog = json.loads(get('/api/templates')); assert len(catalog) >= 245
        for _ in range(40):
            state = json.loads(get('/api/state'))
            if any(n['id']=='local' and n['online'] for n in state['nodes']): break
            time.sleep(.25)
        else: raise RuntimeError('local agent never connected')
        assert isinstance(state['servers'], list) and not state['servers']
        print('PASS: packaged HTML/assets, password login, 245-entry catalog, and detached local host heartbeat')
    finally:
        # Only stop the agent created with this test's unique temporary data path.
        needle = os.fsencode(data+'/local-host')
        for cmdline in pathlib.Path('/proc').glob('[0-9]*/cmdline'):
            try:
                if needle in cmdline.read_bytes().split(b'\0'): os.kill(int(cmdline.parent.name), signal.SIGTERM)
            except (OSError, ValueError): pass
        proc.terminate()
        try: proc.wait(timeout=10)
        except subprocess.TimeoutExpired: proc.kill(); proc.wait()
