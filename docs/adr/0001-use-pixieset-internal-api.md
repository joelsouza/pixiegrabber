# Use Pixieset's internal API

Pixiegrabber calls Pixieset's internal JSON endpoints with an authorized browser session instead of automating a browser. This keeps large Collection discovery and Reference downloads fast and simple, but Pixieset can change the undocumented endpoints without notice, so their request and response handling must stay isolated from the rest of the application.
