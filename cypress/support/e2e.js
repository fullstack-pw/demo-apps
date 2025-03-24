// This file is processed and loaded automatically before your test files
// This is a great place to put global configuration and behavior that modifies Cypress

// Import commands.js using ES2015 syntax:
import './commands';

// Alternatively, use CommonJS syntax:
// require('./commands')

// Hide fetch/XHR requests in the Cypress command log
const app = window.top;
if (!app.document.head.querySelector('[data-hide-command-log-request]')) {
    const style = app.document.createElement('style');
    style.innerHTML = '.command-name-request, .command-name-xhr { display: none }';
    style.setAttribute('data-hide-command-log-request', '');
    app.document.head.appendChild(style);
}

// Configure global behavior
Cypress.on('uncaught:exception', (err, runnable) => {
    // Returning false here prevents Cypress from failing the test
    // This can be useful when testing error handling scenarios
    console.log('Uncaught exception:', err.message);
    return false;
});