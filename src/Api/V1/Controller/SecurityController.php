<?php
declare(strict_types=1);

namespace App\Api\V1\Controller;

use App\Entity\{ApiToken, User};
use App\Repository\{ApiTokenRepository, UserRepository};
use App\Security\ApiTokenAuthenticator;
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};
use Symfony\Component\PasswordHasher\Hasher\UserPasswordHasherInterface;

class SecurityController extends AbstractApiController
{
    public function login(
        Request $request,
        EntityManagerInterface $em,
        UserRepository $userRepo,
        ApiTokenRepository $tokenRepo,
        UserPasswordHasherInterface $passwordHasher,
    ): JsonResponse {
        $username = $request->request->get('username');
        $password = $request->request->get('password');

        /** @var User $user */
        if (null === $user = $userRepo->findOneBy(['username' => $username])) {
            return $this->createJsonResponse(code: JsonResponse::HTTP_UNAUTHORIZED, message: 'User not found');
        }

        if (!$passwordHasher->isPasswordValid($user, $password)) {
            return $this->createJsonResponse(code: JsonResponse::HTTP_UNAUTHORIZED, message: 'Invalid password');
        }

        $apiToken = new ApiToken($user);
        $tokenRepo->add($apiToken);

        try {
            $em->flush();
        } catch (\Exception $ex) {
            return $this->createJsonResponse(code: JsonResponse::HTTP_INTERNAL_SERVER_ERROR, message: 'Token persisting error');
        }

        return $this->createJsonResponse($apiToken->getKey());
    }

    public function logout(Request $request, ApiTokenRepository $apiTokenRepo, EntityManagerInterface $em): JsonResponse {
        if (null === $tokenKey = ApiTokenAuthenticator::getTokenKeyFromRequest($request)) {
            return $this->createJsonResponse(null, code:JsonResponse::HTTP_INTERNAL_SERVER_ERROR, message:  'No API token provided.');
        }

        if (null === $apiToken = $apiTokenRepo->findOneBy(['key' => $tokenKey])) {
            return $this->createJsonResponse(null, code:JsonResponse::HTTP_INTERNAL_SERVER_ERROR, message: 'API token with such key not found in the database.');
        }

        $em->remove($apiToken);

        try {
            $em->flush();
        } catch (\Exception $ex) {
            return $this->createJsonResponse(null,[],JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'API token deauthentication failure.');
        }

        return $this->createJsonResponse(null,[],JsonResponse::HTTP_OK, 'Successfully logged out.');
    }
}