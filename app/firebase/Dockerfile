# Firebase Auth emulator for local development.
FROM node:24-alpine
RUN npm install -g firebase-tools@15.33.0
WORKDIR /firebase
COPY firebase.json .
CMD ["firebase", "emulators:start", "--only", "auth", "--project", "demo-asyncsvc"]
