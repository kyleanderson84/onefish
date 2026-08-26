# Conversation Log

## Redfish Mockup Server Setup

### Server Location
- Path: `/home/kyle_dude/Redfish-Mockup-Server`

### Starting the Server
```bash
cd /home/kyle_dude/Redfish-Mockup-Server
python3 redfishMockupServer.py -D public-rackmount1 -p 8000 -S
```

### Testing onefish
```bash
cd /home/kyle_dude/onefish
./onefish -target localhost:8000
```

### Notes
- Server runs on port 8000 with short-form mode (`-S`)
- Mockup directory: `public-rackmount1`
- onefish successfully authenticates and retrieves Redfish data

## Future Work
- Monitor the server
- Eventually test with containerlab and emulated OpenBMC
