// Places an order and runs its fulfilment workflow on Temporal.
const { Client, Connection } = require('@temporalio/client');
const { Worker, NativeConnection, Runtime, DefaultLogger } = require('@temporalio/worker');
const activities = require('./activities');

Runtime.install({ logger: new DefaultLogger('ERROR') });

(async () => {
  const [item = 'espresso machine', note = null] = process.argv.slice(2);
  const { rows: [order] } = await activities.db.query(
    'INSERT INTO orders (item, gift_note) VALUES ($1, $2) RETURNING id', [item, note]);
  console.log(`order #${order.id}: ${item}${note ? ` (gift note: "${note}")` : ''}`);

  const address = process.env.TEMPORAL_ADDRESS;
  const worker = await Worker.create({
    connection: await NativeConnection.connect({ address }),
    taskQueue: 'orders',
    workflowsPath: require.resolve('./workflows'),
    activities,
  });
  const client = new Client({ connection: await Connection.connect({ address }) });
  await worker.runUntil(client.workflow.execute('fulfillOrder', {
    taskQueue: 'orders', workflowId: `order-${order.id}`, args: [order.id],
  }));
  console.log(`workflow order-${order.id} completed`);
  await activities.db.end();
})().catch(err => { console.error(err.message); process.exit(1); });
