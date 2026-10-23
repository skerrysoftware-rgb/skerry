const { Context } = require('@temporalio/activity');
const { Pool } = require('pg');

const db = new Pool({ connectionString: process.env.DATABASE_URL });

async function setStatus(id, status) {
  await db.query('UPDATE orders SET status = $2 WHERE id = $1', [id, status]);
  console.log(`  ${status}`);
}

module.exports = {
  db,
  reserveStock: id => setStatus(id, 'reserved'),
  async chargeCard(id) {
    // The payment provider times out the first time; Temporal retries.
    if (Context.current().info.attempt === 1) {
      console.log('  payment timed out, retrying');
      throw new Error('payment provider timed out');
    }
    await setStatus(id, 'paid');
  },
  ship: id => setStatus(id, 'shipped'),
};
