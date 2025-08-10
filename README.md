## SIP & WebRTC to OpenAI Gateway (Go)

High-performance Go-based gateway for direct integration of SIP telephony and WebRTC calls with the OpenAI API.
Enables real-time, bidirectional audio and text exchange without intermediate transcoding — ensuring minimal latency, high quality, and seamless integration.

**Features:**

* **SIP** and **WebRTC** support — integrate with both telephony networks and web calls.
* Direct audio packet streaming to the OpenAI API.
* WebM output for playback of AI responses.
* Optimized WebRTC frame storage in an embedded high-performance database.
* Easy embedding into SaaS platforms and enterprise solutions.
* Ready for extension: new formats, additional SIP providers.


---
for install baresip baresip/install.sh

baresip for testing
/dial sip:78632020220@localhost:5060
/dial sip:78632020220@52.14.23.78:5060




## API Methods

### Context Management

*   `POST /context/:phone`: Set context for a phone number.
*   `GET /context/:phone`: Get context for a phone number.
*   `DELETE /context/:phone`: Delete context for a phone number.
*   `GET /contexts`: List all contexts.

### Session and Recording Management

*   `GET /sessions`: List all sessions.
*   `GET /record/:sessionid/:source`: Get session recording. `:source` can be `user` or `assistant`.
*   `DELETE /record/:sessionid`: Delete session recording.
*   `GET /transcript/:sessionid`: Get session transcript.
*   `DELETE /transcript/:sessionid`: Delete session transcript.
*   `GET /user/:user`: Get user sessions.