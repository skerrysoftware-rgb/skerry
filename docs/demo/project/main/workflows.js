const { proxyActivities } = require('@temporalio/workflow');

const { reserveStock, chargeCard, ship } = proxyActivities({
  startToCloseTimeout: '10 seconds',
  retry: { initialInterval: '300 milliseconds', backoffCoefficient: 1 },
});

// Fulfils an order. If a step fails, Temporal retries it; the steps
// before it are not repeated.
async function fulfillOrder(id) {
  await reserveStock(id);
  await chargeCard(id);
  await ship(id);
}

module.exports = { fulfillOrder };
