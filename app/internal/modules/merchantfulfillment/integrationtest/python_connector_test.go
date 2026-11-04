package merchantfulfillment_test

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

// Opt-in verification against the real Python client, using only a temporary SQLite shop.
func TestPythonConnector(t *testing.T) {
	python, botRoot := os.Getenv("UECB_TEST_PYTHON"), os.Getenv("UECB_TEST_BOT_ROOT")
	if python == "" || botRoot == "" {
		t.Skip("set UECB_TEST_PYTHON and UECB_TEST_BOT_ROOT for the cross-language contract test")
	}
	for _, kind := range []string{"card", "link", "physical"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			server := httptest.NewServer(router(f.service))
			defer server.Close()
			command := exec.Command(python, "-c", `
import asyncio, sys, tempfile
from pathlib import Path
from core.fulfillment_sources import BridgeAdapter
from core.fulfillment_runtime import FulfillmentRuntime

async def run():
    source = {'id': 'a'*32, 'name': 'Public isolated fixture', 'platform': 'dujiao', 'shop_id': 'test-shop', 'base_url': sys.argv[1], 'secret': sys.argv[2], 'enabled': True, 'auto_process': False}
    adapter = BridgeAdapter(source)
    health = await adapter.health()
    assert set(health['capabilities']) == {'card', 'link', 'physical'}
    listed = await adapter.list_orders()
    assert 'PUBLIC-TEST-LEAF' in listed['orders']
    order = await adapter.verify_order(adapter.reference('PUBLIC-TEST-LEAF'))
    assert order['status'] == 'paid' and order['amount'] == '6.00'
    line = order['items'][0]
    kind = sys.argv[3]
    job = {key: order[key] for key in ('platform', 'shop_id', 'order_id', 'buyer_id', 'currency')}
    job.update(line, type=kind)
    payload = {'card': {'type':'card','cards':['PUBLIC-DEMO-A','PUBLIC-DEMO-B']}, 'link': {'type':'link','content':'https://example.invalid/demo\n提取码：DEMO'}, 'physical': {'type':'physical','carrier':'示例物流','tracking_number':'PUBLIC-DEMO-TRACK'}}[kind]
    first = await adapter.deliver(job, payload, 'fulfillment:cross-language-demo')
    assert first['status'] == 'sent'
    second = await adapter.deliver(job, payload, 'fulfillment:cross-language-demo')
    assert first == second
    wrong = dict(job, amount='999.00')
    assert await adapter.deliver(wrong, payload, 'fulfillment:cross-language-demo') == {'status':'failed'}
    verified = await adapter.verify_order(adapter.reference('PUBLIC-TEST-LEAF'))
    assert verified['status'] == 'fulfilled'
    # Exercise the actual workbench runtime/store/engine against the second leaf.
    with tempfile.TemporaryDirectory(prefix='uecb-runtime-go-') as directory:
        root = Path(directory)
        runtime = FulfillmentRuntime(root)
        saved = runtime.save_source({key:value for key,value in source.items() if key != 'id'})
        rule = runtime.store.save_rule({'platform':'dujiao','shop_id':'test-shop','item_id':'42','sku_id':'43','type':kind,'enabled':True,'min_unit_amount':'1.00','currency':'CNY', **({'delivery_content':'https://example.invalid/runtime\n提取码：DEMO'} if kind == 'link' else {})})
        if kind == 'card':
            runtime.store.import_cards(rule['id'], ['PUBLIC-RUNTIME-A','PUBLIC-RUNTIME-B'])
        preview = await runtime.sync(saved['id'], dry_run=True)
        assert preview['count'] == 1 and not runtime.store.list_jobs()
        assert preview['results'][0]['jobs'][0]['status'] == ('awaiting_shipment' if kind == 'physical' else 'ready')
        if kind == 'card':
            assert runtime.store.list_rules()[0]['inventory_available'] == 2
        actual = await runtime.sync(saved['id'], dry_run=False)
        assert actual['count'] == 1 and len(runtime.store.list_jobs()) == 1
        job = runtime.store.list_jobs()[0]
        if kind == 'physical':
            assert job['status'] == 'awaiting_shipment'
            assert (await adapter.verify_order(adapter.reference('PUBLIC-TEST-SECOND')))['status'] == 'paid'
            runtime.store.record_tracking(job['id'], '示例物流', 'PUBLIC-RUNTIME-TRACK')
            assert (await runtime.retry(job['id'], dry_run=True))['can_retry'] is True
            assert runtime.store.get_job(job['id'])['status'] == 'awaiting_shipment'
            assert (await runtime.retry(job['id'], dry_run=False))['job']['status'] == 'sent'
        else:
            assert job['status'] == 'sent'
        assert (await runtime.sync(saved['id'], dry_run=False))['count'] == 0
        before = runtime.store.list_rules()[0]['inventory_available']
        assert before == 0
        await runtime.close()
        restarted = FulfillmentRuntime(root)
        assert (await restarted.sync(saved['id'], dry_run=False))['count'] == 0
        assert len(restarted.store.list_jobs()) == 1 and restarted.store.list_jobs()[0]['status'] == 'sent'
        assert restarted.store.list_rules()[0]['inventory_available'] == before
        await restarted.close()
asyncio.run(run())
`, server.URL+"/api/v1/merchant-fulfillment", testSecret, kind)
			command.Dir = botRoot
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("isolated Python/Go contract failed: %v\n%s", err, output)
			}
			if countDeliveries(t, f) != 2 {
				t.Fatal("cross-language retry duplicated fulfillment")
			}
		})
	}
}
