# Firebase Auth + Firestore emulators for local development.
# The Firestore emulator runs on the JVM.
FROM node:24-alpine
RUN apk add --no-cache openjdk21-jre-headless \
 && npm install -g firebase-tools@15.33.0
WORKDIR /firebase
COPY firebase.json firestore.rules ./
CMD ["firebase", "emulators:start", "--only", "auth,firestore", "--project", "demo-asyncsvc"]
