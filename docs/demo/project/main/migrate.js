// Applies migrations/*.sql in order, once each.
const fs = require('fs');
const { Client } = require('pg');

(async () => {
  const db = new Client({ connectionString: process.env.DATABASE_URL });
  await db.connect();
  await db.query('CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY)');
  const done = new Set((await db.query('SELECT name FROM schema_migrations')).rows.map(r => r.name));
  for (const file of fs.readdirSync('migrations').sort()) {
    if (done.has(file)) continue;
    await db.query('BEGIN');
    await db.query(fs.readFileSync(`migrations/${file}`, 'utf8'));
    await db.query('INSERT INTO schema_migrations VALUES ($1)', [file]);
    await db.query('COMMIT');
    console.log(`applied ${file}`);
  }
  const { rows } = await db.query(
    "SELECT string_agg(column_name, ', ' ORDER BY ordinal_position) AS cols FROM information_schema.columns WHERE table_name = 'orders'");
  console.log(`orders(${rows[0].cols})`);
  await db.end();
})().catch(err => { console.error(err.message); process.exit(1); });
