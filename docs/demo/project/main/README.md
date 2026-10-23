# orders

A small order service: Postgres for orders, a Temporal workflow to fulfil them.

    npm run migrate          # apply migrations/*.sql
    npm run order -- ITEM    # place an order and run its workflow
