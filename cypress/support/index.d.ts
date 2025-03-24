// TypeScript definitions for custom Cypress commands

declare namespace Cypress {
    interface Chainable<Subject> {
        /**
         * Send a message to the enqueuer service
         * @param message The message object to send
         * @param queueName The name of the queue to send to
         */
        sendMessage(message: object, queueName?: string): Chainable<Response<any>>;

        /**
         * Verify a message was processed by the memorizer service
         * @param messageId The ID of the message to verify
         * @param timeout Maximum time to wait for processing in milliseconds
         */
        verifyMemorizerProcessed(messageId: string, timeout?: number): Chainable<boolean>;

        /**
         * Verify a message was stored in the database by the writer service
         * @param messageId The ID of the message to verify
         * @param timeout Maximum time to wait for storage in milliseconds
         */
        verifyWriterStored(messageId: string, timeout?: number): Chainable<object>;

        /**
         * Check the health of a service
         * @param service The service name to check (enqueuer, memorizer, writer)
         */
        checkServiceHealth(service: string): Chainable<Response<any>>;

        /**
         * Generate a unique test ID
         * @param prefix Optional prefix for the ID
         */
        generateTestId(prefix?: string): Chainable<string>;

        /**
         * Verify trace context propagation for a message
         * @param messageId The ID of the message to check
         * @param timeout Maximum time to wait for trace data in milliseconds
         */
        verifyTraceContext(messageId: string, timeout?: number): Chainable<string>;
    }
}