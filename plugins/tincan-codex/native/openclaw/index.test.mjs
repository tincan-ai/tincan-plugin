import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import plugin, { OpenClawDelivery, parseWorkerOutcome } from './index.mjs';

function fixture({ fail = false } = {}) {
  const client = new EventEmitter(); const calls = []; let claimed = false;
  client.call = async (method, params) => {
    calls.push({ method, params });
    if (method === 'claim') {
      if (claimed) return { acquired: false };
      claimed = true;
      return { acquired: true, claim: 'private-claim', event: { payload: { text: 'PEER BODY' } } };
    }
    return {};
  };
  client.close = async () => {};
  const runs = [];
  const runtime = { subagent: {
    run: async params => { runs.push(params); return { runId: 'run', sessionKey: params.sessionKey }; },
    waitForRun: async () => ({ status: fail ? 'error' : 'ok' }),
    getSessionMessages: async () => ({ messages: [{ role: 'assistant', content: [{ type: 'text', text: JSON.stringify({status:'completed',reply:'done'}) }] }] }),
  } };
  const delivery = new OpenClawDelivery(client, runtime, { agentId: 'main', scope: 'Review only' }, { error() {}, warn() {} });
  delivery.connection = 'conn';
  return { client, calls, runs, delivery };
}
test('isolated worker commits only after completion, duplicate notices do not rerun', async () => {
  const { delivery, calls, runs } = fixture();
  await Promise.all([delivery.mention({ connection: 'conn', event_seq: 7 }), delivery.mention({ connection: 'conn', event_seq: 7 })]);
  assert.equal(runs.length, 1);
  assert.match(runs[0].sessionKey, /^agent:main:subagent:tincan-/);
  assert.equal(runs[0].deliver, false);
  assert.ok(!runs[0].message.includes('private-claim'));
  assert.deepEqual(calls.find(c => c.method === 'outcome'), { method: 'outcome', params: { connection: 'conn', seq: 7, claim: 'private-claim', outcome: {status:'completed',reply:'done'} } });
  await delivery.mention({ connection: 'conn', event_seq: 7 });
  assert.equal(runs.length, 1);
});
test('failures, wrong connections, and nonmentions cannot acknowledge work', async () => {
  const { delivery, calls, client, runs } = fixture({ fail: true });
  await delivery.mention({ connection: 'other', event_seq: 7 });
  client.emit('notice', { event: 'paired', data: { connection: 'conn' } });
  client.emit('notice', { event: 'join_request', data: { connection: 'conn' } });
  assert.equal(runs.length, 0);
  await assert.rejects(delivery.mention({ connection: 'conn', event_seq: 7 }), /failed/);
  assert.equal(calls.filter(c => ['ack', 'reply', 'release'].includes(c.method)).length, 0);
});
test('plugin registration is inert, service starts only after scope configuration', async () => {
  let service;
  plugin.register({ pluginConfig: {}, registerService(value) { service = value; } });
  assert.equal(service.id, 'tincan-listener');
  await service.start({ logger: { info() {} } });
  await service.stop();
});

test('final prose is not completion and permission requests stay structured', () => {
  for (const value of ['done', 'I need approval', '{"status":"queued"}', '{"status":"awaiting_approval"}']) assert.throws(() => parseWorkerOutcome(value));
  const waiting={status:'awaiting_approval',question:'May I inspect the data?',context:'Corrected denominator'};
  assert.deepEqual(parseWorkerOutcome(JSON.stringify(waiting)),waiting);
});
test('uncertain worker does not stop independent future work', async () => {
  const {delivery,calls}=fixture({fail:true});
  await assert.rejects(delivery.mention({connection:'conn',event_seq:7}));
  assert.equal(delivery.stopping,false);
  assert.equal(calls.find(c=>c.method==='outcome').params.outcome.status,'needs_recovery');
});

test('user presentation is recorded only after an explicit host receipt', async () => {
  for (const presented of [false,true]) {
    const calls=[];const client=new EventEmitter();
    client.call=async(method,params)=>{calls.push({method,params});return method==='requests'?{requests:[{event:{seq:1},approval:{id:'decision-1',question:'May I inspect data?',delivery:'pending'}}]}:{}};
    const delivery=new OpenClawDelivery(client,{}, {}, {warn(){},error(){}},async()=>{},async()=>({presented}));
    delivery.connection='conn';await delivery.reportDecisions();await delivery.reportDecisions();
    assert.equal(calls.filter(c=>c.method==='decide').length,presented?1:0);
  }
});

for (const kind of ['message', 'mention']) {
  test(`${kind} notices dispatch and complete a worker`, async () => {
    const {client, delivery, calls, runs} = fixture();
    client.emit('notice', {event:kind, data:{connection:'conn',event_seq:7}});
    await Promise.all([...delivery.jobs]);
    assert.equal(runs.length,1);
    assert.equal(calls.filter(c => c.method === 'outcome').length,1);
  });
}
